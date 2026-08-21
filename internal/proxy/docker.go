package proxy

import (
	"archive/tar"
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
	"github.com/hansputera/opencode-multi-agents/internal/config"
	"github.com/rs/zerolog"
	"golang.org/x/net/proxy"
)

// vpnImageTag is the tag of the ProtonVPN image with gost SOCKS5 sidecar,
// built on demand by ensureVPNImage at startup.
const vpnImageTag = "opencode-multi-agents/protonvpn:latest"

// protonwireGostDockerfile is embedded so the gateway can build the VPN image
// at runtime without shipping Dockerfiles on disk.
//
//go:embed assets/protonwire-gost.Dockerfile
var protonwireGostDockerfile string

// protonwireEntrypoint is the entrypoint script embedded alongside the
// Dockerfile so the build context is self-contained.
//
//go:embed assets/protonwire-entrypoint.sh
var protonwireEntrypoint string

// DockerManager manages VPN containers using Docker SDK
type DockerManager struct {
	cli       *client.Client
	cfg       *config.Config
	log       *zerolog.Logger
	nextPort  atomic.Int32
	namespace string // Container name prefix
	imageMu   sync.Mutex
	imageDone bool
	keyIndex  atomic.Int32
	vpnOnce   sync.Once
	vpnErr    error
}

// NewDockerManager creates a new Docker manager
func NewDockerManager(cfg *config.Config, log *zerolog.Logger) (*DockerManager, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	dm := &DockerManager{
		cli:       cli,
		cfg:       cfg,
		log:       log,
		namespace: "protonvpn-gateway",
	}
	dm.nextPort.Store(int32(cfg.ProxyBasePort))

	// Clean up existing containers from previous runs. Must complete before
	// the pool starts creating containers to avoid name conflicts.
	dm.cleanupOrphans(context.Background())

	// Build the VPN+gost image synchronously so pool.Start can create
	// containers immediately.
	buildCtx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := dm.ensureVPNImage(buildCtx); err != nil {
		return nil, fmt.Errorf("failed to prepare VPN image: %w", err)
	}

	return dm, nil
}

// ensureVPNImage makes sure the ProtonVPN+gost image exists locally,
// building it from the embedded Dockerfile on first use. Safe for concurrent
// callers (sync.Once).
func (dm *DockerManager) ensureVPNImage(ctx context.Context) error {
	dm.vpnOnce.Do(func() {
		dm.log.Info().Str("image", vpnImageTag).Msg("Ensuring ProtonVPN+gost image")

		if _, _, err := dm.cli.ImageInspectWithRaw(ctx, vpnImageTag); err == nil {
			dm.log.Info().Str("image", vpnImageTag).Msg("ProtonVPN+gost image already present")
			return
		}

		dm.log.Info().Str("image", vpnImageTag).Msg("Building ProtonVPN+gost image...")

		// Build context: Dockerfile + entrypoint script
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.WriteHeader(&tar.Header{
			Name: "protonwire-gost.Dockerfile",
			Mode: 0o644,
			Size: int64(len(protonwireGostDockerfile)),
		})
		_, _ = tw.Write([]byte(protonwireGostDockerfile))
		_ = tw.WriteHeader(&tar.Header{
			Name: "protonwire-entrypoint.sh",
			Mode: 0o755,
			Size: int64(len(protonwireEntrypoint)),
		})
		_, _ = tw.Write([]byte(protonwireEntrypoint))
		_ = tw.Close()

		resp, err := dm.cli.ImageBuild(ctx, &buf, types.ImageBuildOptions{
			Tags:        []string{vpnImageTag},
			Dockerfile:  "protonwire-gost.Dockerfile",
			Remove:      true,
			ForceRemove: true,
		})
		if err != nil {
			dm.vpnErr = fmt.Errorf("VPN image build failed: %w", err)
			return
		}
		defer resp.Body.Close()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			dm.vpnErr = fmt.Errorf("VPN image build: %w", err)
			return
		}

		dm.log.Info().Str("image", vpnImageTag).Msg("ProtonVPN+gost image built")
	})
	return dm.vpnErr
}

// Exec runs a command inside a proxy container (docker exec) as the container's
// default user and returns the combined stdout+stderr output. Used by the
// opencode-cli upstream driver to run `opencode run` inside each container.
func (dm *DockerManager) Exec(ctx context.Context, containerID string, env, args []string) ([]byte, error) {
	execCfg := container.ExecOptions{
		AttachStdout: true,
		AttachStderr: true,
		Env:          env,
		Cmd:          args,
	}

	idResp, err := dm.cli.ContainerExecCreate(ctx, containerID, execCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create exec: %w", err)
	}

	attach, err := dm.cli.ContainerExecAttach(ctx, idResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to attach to exec: %w", err)
	}
	defer attach.Close()

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, attach.Reader); err != nil {
		return nil, fmt.Errorf("failed to read exec output: %w", err)
	}

	inspect, err := dm.cli.ContainerExecInspect(ctx, idResp.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect exec: %w", err)
	}

	out := append(stdout.Bytes(), stderr.Bytes()...)
	if inspect.ExitCode != 0 {
		return out, fmt.Errorf("exec exited with code %d (command: %s)", inspect.ExitCode, strings.Join(args, " "))
	}
	return out, nil
}

// ensureImage makes sure the VPN image is present locally, pulling it if needed.
func (dm *DockerManager) ensureImage(ctx context.Context) error {
	dm.imageMu.Lock()
	defer dm.imageMu.Unlock()

	if dm.imageDone {
		return nil
	}

	_, _, err := dm.cli.ImageInspectWithRaw(ctx, vpnImageTag)
	if err == nil {
		dm.imageDone = true
		return nil
	}
	if !client.IsErrNotFound(err) {
		return fmt.Errorf("failed to inspect image: %w", err)
	}

	dm.log.Info().Str("image", vpnImageTag).Msg("Pulling VPN image...")
	reader, err := dm.cli.ImagePull(ctx, vpnImageTag, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("failed to pull image %s: %w", vpnImageTag, err)
	}
	defer reader.Close()

	// Drain the pull stream to wait for completion
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return fmt.Errorf("failed to pull image %s: %w", vpnImageTag, err)
	}

	dm.log.Info().Str("image", vpnImageTag).Msg("VPN image pulled")
	dm.imageDone = true
	return nil
}

// Create creates a new VPN container
func (dm *DockerManager) Create(ctx context.Context) (*Proxy, error) {
	return dm.CreateEx(ctx, nil)
}

// CreateEx creates a new VPN container, avoiding the given banned regions.
func (dm *DockerManager) CreateEx(ctx context.Context, bannedRegions map[string]bool) (*Proxy, error) {
	// Make sure the VPN image is available locally first
	if err := dm.ensureImage(ctx); err != nil {
		return nil, fmt.Errorf("VPN image unavailable: %w", err)
	}

	// Select key file (prefer unbanned regions)
	keyFile, region, err := dm.nextKeyFile(bannedRegions)
	if err != nil {
		return nil, fmt.Errorf("no available key files: %w", err)
	}

	port := int(dm.nextPort.Add(1))
	containerName := fmt.Sprintf("%s-%d", dm.namespace, port)

	// Read the private key content for the environment variable
	keyContent, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read key file %s: %w", keyFile, err)
	}
	privateKey := strings.TrimSpace(string(keyContent))

	// Container configuration
	containerConfig := &container.Config{
		Image: vpnImageTag,
		Env: []string{
			fmt.Sprintf("PROTONVPN_SERVER=%s", dm.cfg.ProtonVPNServer),
			fmt.Sprintf("WIREGUARD_PRIVATE_KEY=%s", privateKey),
			"SKIP_DNS_CONFIG=1",
		},
		ExposedPorts: nat.PortSet{
			"1080/tcp": struct{}{},
		},
		Labels: map[string]string{
			"protonvpn-gateway": "true",
			"protonvpn-port":    fmt.Sprintf("%d", port),
			"protonvpn-region":  region,
		},
	}

	// Host configuration with port binding
	hostConfig := &container.HostConfig{
		PortBindings: nat.PortMap{
			"1080/tcp": []nat.PortBinding{
				{HostIP: "127.0.0.1", HostPort: fmt.Sprintf("%d", port)},
			},
		},
		Resources: container.Resources{
			CPUQuota:   dm.parseCPUQuota(dm.cfg.ResourceCPULimit),
			Memory:     dm.parseMemoryLimit(dm.cfg.ResourceMemoryLimit),
			MemorySwap: dm.parseMemoryLimit(dm.cfg.ResourceMemoryLimit),
		},
		// ProtonVPN needs NET_ADMIN for WireGuard interface management
		CapAdd: []string{"NET_ADMIN"},
		Sysctls: map[string]string{
			"net.ipv4.conf.all.rp_filter":    "2",
			"net.ipv6.conf.all.disable_ipv6": "1",
		},
		AutoRemove: false, // We manage removal manually
	}

	// Network configuration
	networkConfig := &network.NetworkingConfig{}

	// Create container
	resp, err := dm.cli.ContainerCreate(ctx,
		containerConfig,
		hostConfig,
		networkConfig,
		nil,
		containerName,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create container: %w", err)
	}

	// Start container
	if err := dm.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// Clean up created container
		_ = dm.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return nil, fmt.Errorf("failed to start container: %w", err)
	}

	proxy := &Proxy{
		ID:          resp.ID[:12],
		ContainerID: resp.ID,
		SOCKS5Addr:  fmt.Sprintf("socks5://127.0.0.1:%d", port),
		Port:        port,
		State:       StateIdle,
		CreatedAt:   time.Now(),
		LastCheck:   time.Now(),
		Region:      region,
		KeyFile:     keyFile,
	}

	// Wait for container to be ready
	if err := dm.waitForReady(ctx, proxy); err != nil {
		// Clean up unready container
		_ = dm.Remove(ctx, resp.ID)
		return nil, fmt.Errorf("container not ready: %w", err)
	}

	return proxy, nil
}

// waitForReady waits for the VPN container to be ready by checking the
// SOCKS5 proxy health via the IP check endpoint.
func (dm *DockerManager) waitForReady(ctx context.Context, proxy *Proxy) error {
	// VPN needs time to establish WireGuard tunnel
	timeout := time.After(120 * time.Second)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			healthy, _ := dm.HealthCheck(ctx, proxy)
			if healthy {
				return nil
			}
		case <-timeout:
			return fmt.Errorf("timeout waiting for container to be ready")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Remove removes a VPN container
func (dm *DockerManager) Remove(ctx context.Context, containerID string) error {
	if containerID == "" {
		return nil
	}

	dm.log.Debug().Str("container_id", containerID[:12]).Msg("Removing container")

	err := dm.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{
		Force:         true,
		RemoveVolumes: true,
	})

	if err != nil {
		return fmt.Errorf("failed to remove container: %w", err)
	}

	return nil
}

// HealthCheck checks if the VPN container is healthy by tracing
// through its SOCKS5 proxy and verifying the egress IP is reachable.
func (dm *DockerManager) HealthCheck(ctx context.Context, p *Proxy) (bool, error) {
	// Dial through the container's SOCKS5 proxy so the trace reflects
	// the VPN container's egress, not the gateway host's.
	addr := strings.TrimPrefix(p.SOCKS5Addr, "socks5://")
	dialer, err := proxy.SOCKS5("tcp", addr, nil, proxy.Direct)
	if err != nil {
		return false, fmt.Errorf("failed to create SOCKS5 dialer: %w", err)
	}

	transport := &http.Transport{Dial: dialer.Dial}
	client := &http.Client{
		Transport: transport,
		Timeout:   15 * time.Second,
	}

	// Check via IP check endpoint
	checkURL := dm.cfg.ProtonVPNIPCheckURL
	if checkURL == "" {
		checkURL = "https://icanhazip.com/"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", checkURL, nil)
	if err != nil {
		return false, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}

	ip := strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		return false, fmt.Errorf("invalid IP response: %s", ip)
	}

	p.EgressIP = ip
	return true, nil
}

// cleanupOrphans removes containers from previous runs
func (dm *DockerManager) cleanupOrphans(ctx context.Context) {
	filter := filters.NewArgs()
	filter.Add("label", "protonvpn-gateway=true")

	containers, err := dm.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filter,
	})
	if err != nil {
		dm.log.Debug().Err(err).Msg("Failed to list orphan containers")
		return
	}

	for _, c := range containers {
		if err := dm.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{
			Force:         true,
			RemoveVolumes: true,
		}); err != nil {
			dm.log.Debug().Err(err).Str("container_id", c.ID[:12]).Msg("Failed to remove orphan container")
		} else {
			dm.log.Info().Str("container_id", c.ID[:12]).Msg("Removed orphan container")
		}
	}
}

// Close closes the Docker client
func (dm *DockerManager) Close() error {
	return dm.cli.Close()
}

// nextKeyFile selects the next key file from the configured directory,
// preferring regions that are not currently banned.
func (dm *DockerManager) nextKeyFile(bannedRegions map[string]bool) (string, string, error) {
	if dm.cfg.ProtonVPNPrivateKeyDir == "" {
		return "", "", fmt.Errorf("PROTONVPN_PRIVATE_KEY_DIR is not configured")
	}

	files, err := os.ReadDir(dm.cfg.ProtonVPNPrivateKeyDir)
	if err != nil {
		return "", "", fmt.Errorf("failed to read key directory: %w", err)
	}

	// Collect key files and their regions
	type keyEntry struct {
		path   string
		region string
	}
	var allKeys []keyEntry
	var availableKeys []keyEntry

	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".key") {
			continue
		}
		region := strings.TrimSuffix(f.Name(), ".key")
		entry := keyEntry{
			path:   filepath.Join(dm.cfg.ProtonVPNPrivateKeyDir, f.Name()),
			region: region,
		}
		allKeys = append(allKeys, entry)
		if bannedRegions == nil || !bannedRegions[region] {
			availableKeys = append(availableKeys, entry)
		}
	}

	if len(allKeys) == 0 {
		return "", "", fmt.Errorf("no .key files found in %s", dm.cfg.ProtonVPNPrivateKeyDir)
	}

	// Prefer unbanned regions, fallback to all if all banned
	keys := availableKeys
	if len(keys) == 0 {
		dm.log.Warn().Msg("All regions banned, using any available key")
		keys = allKeys
	}

	// Round-robin selection
	idx := int(dm.keyIndex.Add(1)-1) % len(keys)
	selected := keys[idx]

	dm.log.Debug().
		Str("region", selected.region).
		Str("key_file", filepath.Base(selected.path)).
		Msg("Selected VPN key")

	return selected.path, selected.region, nil
}

// parseCPUQuota parses CPU limit string to quota
// e.g., "0.25" -> 25000 (25% of CPU)
func (dm *DockerManager) parseCPUQuota(cpu string) int64 {
	var cores float64
	if _, err := fmt.Sscanf(cpu, "%f", &cores); err != nil {
		return 0
	}
	return int64(cores * 100000)
}

// parseMemoryLimit parses memory limit string to bytes
// e.g., "64M" -> 67108864
func (dm *DockerManager) parseMemoryLimit(mem string) int64 {
	var value int64
	var unit string
	if _, err := fmt.Sscanf(mem, "%d%s", &value, &unit); err != nil {
		return 0
	}

	switch strings.ToUpper(unit) {
	case "K", "KB":
		return value * 1024
	case "M", "MB":
		return value * 1024 * 1024
	case "G", "GB":
		return value * 1024 * 1024 * 1024
	default:
		return value
	}
}
