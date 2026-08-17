package proxy

import (
	"archive/tar"
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"net/http"
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

// cliImageTag is the tag of the WARP image with the opencode CLI baked in,
// built on demand by ensureCLIImage when UPSTREAM_PROVIDER=opencode-cli.
const cliImageTag = "opencode-multi-agents/warp-opencode:latest"

// opencodeWarpDockerfile is embedded so the gateway can build the CLI image
// at runtime without shipping Dockerfiles on disk.
//
//go:embed assets/opencode-warp.Dockerfile
var opencodeWarpDockerfile string

// DockerManager manages WARP containers using Docker SDK
type DockerManager struct {
	cli       *client.Client
	cfg       *config.Config
	log       *zerolog.Logger
	nextPort  atomic.Int32
	namespace string // Container name prefix
	imageMu   sync.Mutex
	imageDone bool
	cliOnce   sync.Once
	cliErr    error
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
		namespace: "warp-gateway",
	}
	dm.nextPort.Store(int32(cfg.ProxyBasePort))

	// Clean up existing containers from previous runs. Must complete before
	// the pool starts creating containers to avoid name conflicts.
	dm.cleanupOrphans(context.Background())

	// opencode-cli mode requires the baked image; build it synchronously so
	// pool.Start creates containers from the right image.
	if cfg.UpstreamProvider == "opencode-cli" {
		buildCtx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		if err := dm.ensureCLIImage(buildCtx); err != nil {
			return nil, fmt.Errorf("failed to prepare opencode CLI image: %w", err)
		}
		dm.cfg.WARPImage = cliImageTag
	}

	// Pull the WARP image in the background so containers can start immediately
	go func() {
		pullCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := dm.ensureImage(pullCtx); err != nil {
			dm.log.Error().Err(err).Str("image", cfg.WARPImage).Msg("Failed to ensure WARP image")
		}
	}()

	return dm, nil
}

// ensureCLIImage makes sure the opencode CLI WARP image exists locally,
// building it from the embedded Dockerfile on first use. Safe for concurrent
// callers (sync.Once).
func (dm *DockerManager) ensureCLIImage(ctx context.Context) error {
	dm.cliOnce.Do(func() {
		dm.log.Info().Str("image", cliImageTag).Msg("Ensuring opencode CLI WARP image")

		if _, _, err := dm.cli.ImageInspectWithRaw(ctx, cliImageTag); err == nil {
			dm.log.Info().Str("image", cliImageTag).Msg("opencode CLI WARP image already present")
			return
		}

		dm.log.Info().Str("image", cliImageTag).Msg("Building opencode CLI WARP image (Node 20 + opencode-ai)...")

		// Minimal build context: a single Dockerfile.
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.WriteHeader(&tar.Header{
			Name: "opencode-warp.Dockerfile",
			Mode: 0o644,
			Size: int64(len(opencodeWarpDockerfile)),
		})
		_, _ = tw.Write([]byte(opencodeWarpDockerfile))
		_ = tw.Close()

		resp, err := dm.cli.ImageBuild(ctx, &buf, types.ImageBuildOptions{
			Tags:        []string{cliImageTag},
			Dockerfile:  "opencode-warp.Dockerfile",
			Remove:      true,
			ForceRemove: true,
		})
		if err != nil {
			dm.cliErr = fmt.Errorf("opencode CLI image build failed: %w", err)
			return
		}
		defer resp.Body.Close()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			dm.cliErr = fmt.Errorf("opencode CLI image build: %w", err)
			return
		}

		dm.log.Info().Str("image", cliImageTag).Msg("opencode CLI WARP image built")
	})
	return dm.cliErr
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

// ensureImage makes sure the WARP image is present locally, pulling it if needed.
func (dm *DockerManager) ensureImage(ctx context.Context) error {
	dm.imageMu.Lock()
	defer dm.imageMu.Unlock()

	if dm.imageDone {
		return nil
	}

	_, _, err := dm.cli.ImageInspectWithRaw(ctx, dm.cfg.WARPImage)
	if err == nil {
		dm.imageDone = true
		return nil
	}
	if !client.IsErrNotFound(err) {
		return fmt.Errorf("failed to inspect image: %w", err)
	}

	dm.log.Info().Str("image", dm.cfg.WARPImage).Msg("Pulling WARP image...")
	reader, err := dm.cli.ImagePull(ctx, dm.cfg.WARPImage, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("failed to pull image %s: %w", dm.cfg.WARPImage, err)
	}
	defer reader.Close()

	// Drain the pull stream to wait for completion
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return fmt.Errorf("failed to pull image %s: %w", dm.cfg.WARPImage, err)
	}

	dm.log.Info().Str("image", dm.cfg.WARPImage).Msg("WARP image pulled")
	dm.imageDone = true
	return nil
}

// Create creates a new WARP container
func (dm *DockerManager) Create(ctx context.Context) (*Proxy, error) {
	// Make sure the WARP image is available locally first
	if err := dm.ensureImage(ctx); err != nil {
		return nil, fmt.Errorf("WARP image unavailable: %w", err)
	}

	port := int(dm.nextPort.Add(1))
	containerName := fmt.Sprintf("%s-%d", dm.namespace, port)

	// Container configuration. Env must stay nil: setting it to an empty
	// slice replaces the image's ENV (PATH, HOME, ...), which would break
	// `docker exec opencode ...` in CLI mode.
	containerConfig := &container.Config{
		Image:        dm.cfg.WARPImage,
		ExposedPorts: nat.PortSet{
			"1080/tcp": struct{}{},
		},
		Labels: map[string]string{
			"warp-gateway": "true",
			"warp-port":    fmt.Sprintf("%d", port),
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
		// WARP needs NET_ADMIN to manage its TUN interface
		CapAdd: []string{"NET_ADMIN"},
		Sysctls: map[string]string{
			"net.ipv6.conf.all.disable_ipv6": "0",
			"net.ipv4.conf.all.src_valid_mark": "1",
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
	}

	// Wait for container to be ready
	if err := dm.waitForReady(ctx, proxy); err != nil {
		// Clean up unready container
		_ = dm.Remove(ctx, resp.ID)
		return nil, fmt.Errorf("container not ready: %w", err)
	}

	return proxy, nil
}

// waitForReady waits for the WARP container to be ready. In opencode-cli mode
// it also verifies the opencode CLI is present inside the container.
func (dm *DockerManager) waitForReady(ctx context.Context, proxy *Proxy) error {
	// WARP needs time on first boot to register and establish the tunnel.
	timeout := time.After(120 * time.Second)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			healthy, _ := dm.HealthCheck(ctx, proxy)
			if !healthy {
				continue
			}
			if dm.cfg.UpstreamProvider == "opencode-cli" {
				if _, err := dm.Exec(ctx, proxy.ContainerID, nil, []string{"opencode", "--version"}); err != nil {
					dm.log.Warn().Err(err).Str("proxy_id", proxy.ID).Msg("opencode CLI not ready yet")
					continue
				}
			}
			return nil
		case <-timeout:
			return fmt.Errorf("timeout waiting for container to be ready")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Remove removes a WARP container
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

// HealthCheck checks if the WARP container is healthy by tracing
// through its SOCKS5 proxy and verifying WARP is enabled.
func (dm *DockerManager) HealthCheck(ctx context.Context, p *Proxy) (bool, error) {
	// Dial through the container's SOCKS5 proxy so the trace reflects
	// the WARP container's egress, not the gateway host's.
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

	// Check via cloudflare trace endpoint
	req, err := http.NewRequestWithContext(ctx, "GET", "https://cloudflare.com/cdn-cgi/trace", nil)
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

	// Check for warp=on in response
	trace := string(body)
	p.EgressIP = egressIPFromTrace(trace)
	if strings.Contains(trace, "warp=on") {
		return true, nil
	}

	return false, fmt.Errorf("WARP not enabled")
}

// egressIPFromTrace extracts the public egress IP ("ip=" line) from a
// cloudflare cdn-cgi/trace response.
func egressIPFromTrace(trace string) string {
	for _, line := range strings.Split(trace, "\n") {
		if strings.HasPrefix(line, "ip=") {
			return strings.TrimPrefix(line, "ip=")
		}
	}
	return ""
}

// cleanupOrphans removes containers from previous runs
func (dm *DockerManager) cleanupOrphans(ctx context.Context) {
	filter := filters.NewArgs()
	filter.Add("label", "warp-gateway=true")

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
