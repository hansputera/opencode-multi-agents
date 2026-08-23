package metrics

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SystemInfo describes the host + process: the "server specifications"
// section of the dashboard. Static facts (CPU model, core count, OS) and
// live gauges (memory, load, goroutines) are collected together; callers
// poll this on every /api/metrics request — all reads are cheap.
type SystemInfo struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	GoVersion string `json:"go_version"`

	NumCPU   int    `json:"num_cpu"`
	CPUModel string `json:"cpu_model,omitempty"`

	// Host memory (Linux /proc/meminfo; zeros when unavailable).
	MemTotalMB uint64 `json:"mem_total_mb"`
	MemAvailMB uint64 `json:"mem_avail_mb"`

	// Load averages 1/5/15 (Linux).
	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`

	HostUptimeSeconds int64 `json:"host_uptime_seconds"`

	// Process vitals.
	ProcRSSMB     float64 `json:"proc_rss_mb"`
	HeapAllocMB   float64 `json:"heap_alloc_mb"`
	SysMemMB      float64 `json:"sys_mem_mb"`
	Goroutines    int     `json:"goroutines"`
	ProcessUptime float64 `json:"process_uptime_s"`

	// Disk holding the data directory.
	DiskTotalGB float64 `json:"disk_total_gb"`
	DiskFreeGB  float64 `json:"disk_free_gb"`
}

var processStart = time.Now()

// CollectSystemInfo gathers host + process specifications. dataDir is used
// for the disk-usage gauge ("" skips it).
func CollectSystemInfo(dataDir string) SystemInfo {
	info := SystemInfo{
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		GoVersion:     runtime.Version(),
		NumCPU:        runtime.NumCPU(),
		ProcessUptime: time.Since(processStart).Seconds(),
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	info.HeapAllocMB = float64(ms.HeapAlloc) / (1 << 20)
	info.SysMemMB = float64(ms.Sys) / (1 << 20)
	info.Goroutines = runtime.NumGoroutine()

	if runtime.GOOS == "linux" {
		info.CPUModel = cpuModelLinux()
		readMemInfo(&info)
		readLoadAvg(&info)
		readUptime(&info)
		readRSS(&info)
	}
	if dataDir != "" {
		diskUsage(dataDir, &info)
	}
	return info
}

func cpuModelLinux() string {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "model name") {
			if _, v, ok := strings.Cut(line, ":"); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

func readMemInfo(info *SystemInfo) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		kv := strings.SplitN(line, ":", 2)
		if len(kv) != 2 {
			continue
		}
		kb, _ := strconv.ParseUint(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(kv[1]), " kB")), 10, 64)
		switch kv[0] {
		case "MemTotal":
			info.MemTotalMB = kb / 1024
		case "MemAvailable":
			info.MemAvailMB = kb / 1024
		}
	}
}

func readLoadAvg(info *SystemInfo) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return
	}
	fields := strings.Fields(string(b))
	if len(fields) >= 3 {
		info.Load1, _ = strconv.ParseFloat(fields[0], 64)
		info.Load5, _ = strconv.ParseFloat(fields[1], 64)
		info.Load15, _ = strconv.ParseFloat(fields[2], 64)
	}
}

func readUptime(info *SystemInfo) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return
	}
	f := strings.Fields(string(b))
	if len(f) >= 1 {
		up, _ := strconv.ParseFloat(f[0], 64)
		info.HostUptimeSeconds = int64(up)
	}
}

func readRSS(info *SystemInfo) {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			kb, _ := strconv.ParseUint(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(line, "VmRSS:")), " kB")), 10, 64)
			info.ProcRSSMB = float64(kb) / 1024
			return
		}
	}
}

func diskUsage(dir string, info *SystemInfo) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	for {
		var st syscall.Statfs_t
		if err := syscall.Statfs(abs, &st); err == nil {
			total := st.Blocks * uint64(st.Bsize)
			free := st.Bavail * uint64(st.Bsize)
			info.DiskTotalGB = float64(total) / (1 << 30)
			info.DiskFreeGB = float64(free) / (1 << 30)
			return
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return
		}
		abs = parent
	}
}

// SortEndpoints is a small helper kept for tests.
func SortEndpoints(e []EndpointUsage) {
	sort.Slice(e, func(i, j int) bool { return e[i].Requests > e[j].Requests })
}
