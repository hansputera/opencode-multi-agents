package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/hansputera/opencode-multi-agents/internal/config"
	"github.com/rs/zerolog"
	"golang.org/x/net/proxy"
)

// DockerManager manages WARP containers using Docker SDK
type DockerManager struct {
	cli       *client.Client
	cfg       *config.Config
	log       *zerolog.Logger
	nextPort  atomic.Int32
	namespace string // Container name prefix
	imageMu   sync.Mutex
	imageDone bool
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

	// Container configuration
	containerConfig := &container.Config{
		Image: dm.cfg.WARPImage,
		ExposedPorts: nat.PortSet{
			"1080/tcp": struct{}{},
		},
		Env: []string{},
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

// waitForReady waits for the WARP container to be ready
func (dm *DockerManager) waitForReady(ctx context.Context, proxy *Proxy) error {
	// WARP needs time on first boot to register and establish the tunnel.
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
