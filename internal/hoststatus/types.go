// Package hoststatus samples host pressure (CPU, memory, physical disk
// activity, free space, and optional build-cache sizes) and folds it into
// one verdict an orchestrator can act on before starting parallel work.
//
// Sampling is read-only and never reaches the network. A metric that cannot
// be sampled is reported as null with a reason; it is never fabricated.
package hoststatus

import "time"

// Verdict is the single action an orchestrator should take.
type Verdict string

// Report sources.
const (
	SourceWatcher = "watcher"
	SourceSampled = "sampled"
)

// Verdicts, from least to most severe.
const (
	VerdictGo       Verdict = "go"
	VerdictThrottle Verdict = "throttle"
	VerdictUnknown  Verdict = "unknown"
	VerdictClean    Verdict = "clean"
	VerdictHold     Verdict = "hold"
)

// severity orders verdicts: hold > clean > unknown > throttle > go.
func (v Verdict) severity() int {
	switch v {
	case VerdictThrottle:
		return 1
	case VerdictUnknown:
		return 2
	case VerdictClean:
		return 3
	case VerdictHold:
		return 4
	default:
		return 0
	}
}

// Disk scopes.
const (
	ScopeHost        = "host"
	ScopeWSLGuest    = "wsl-guest"
	ScopeUnavailable = "unavailable"
)

// Thresholds used by Evaluate. They are variables so LoadConfig can
// override them from the shared config.json (same keys as the Python scripts).
var (
	DiskHoldActivePct     = 90.0
	DiskHoldLatencyMs     = 100.0
	DiskThrottleActivePct = 70.0
	DiskThrottleLatencyMs = 50.0
	CPUHoldPct            = 95.0
	CPUThrottlePct        = 85.0
	MemoryHoldPct         = 95.0
	MemoryThrottlePct     = 88.0
	DefaultMinFreeGB      = 25.0
)

// Sampling defaults and units.
const (
	DefaultSamples       = 3
	DefaultInterval      = time.Second
	DefaultCacheFileName = "host-watcher-status.json"
	bytesPerGiB          = 1 << 30
	bytesPerMB           = 1e6
)

// Report is the host.status payload.
type Report struct {
	Verdict   Verdict  `json:"verdict"`
	Reasons   []string `json:"reasons"`
	SampledAt string   `json:"sampled_at"`
	// Source is "watcher" when the report is the watcher's snapshot and
	// "sampled" when this invocation sampled the host itself.
	Source string `json:"source"`
	OS     string `json:"os"`
	// SnapshotPath is where the watcher writes its snapshot; hooks read it.
	SnapshotPath string   `json:"snapshot_path,omitempty"`
	CPU          *CPU     `json:"cpu"`
	Memory       *Memory  `json:"memory"`
	Disk         *Disk    `json:"disk"`
	Space        []Space  `json:"space"`
	TopIO        []ProcIO `json:"top_io"`
	// IOScope qualifies top_io rates when they are not disk-only:
	// TopIOScopeAllIO on Windows, empty elsewhere.
	IOScope string `json:"io_scope,omitempty"`
	// Inbox is the caller's worktree inbox entry, read without consuming it.
	Inbox  *InboxEntry `json:"inbox,omitempty"`
	Caches []Cache     `json:"caches,omitempty"`
}

// CPU is the median busy percentage over the sampled intervals.
type CPU struct {
	BusyPct float64 `json:"busy_pct"`
	Cores   int     `json:"cores"`
}

// Memory is physical memory use at sampling time.
type Memory struct {
	UsedPct     float64 `json:"used_pct"`
	AvailableGB float64 `json:"available_gb"`
	TotalGB     float64 `json:"total_gb"`
}

// Disk is the busiest physical disk (median of samples). Fields the
// platform cannot measure stay nil.
type Disk struct {
	Device        string   `json:"device"`
	ActivePct     *float64 `json:"active_pct"`
	LatencyMs     *float64 `json:"latency_ms"`
	WriteMBs      *float64 `json:"write_mb_s"`
	ReadMBs       *float64 `json:"read_mb_s"`
	ThroughputMBs *float64 `json:"throughput_mb_s,omitempty"`
	Scope         string   `json:"scope"`
}

// Space is free space on one watched volume.
type Space struct {
	Path    string  `json:"path"`
	FreeGB  float64 `json:"free_gb"`
	TotalGB float64 `json:"total_gb"`
	Low     bool    `json:"low"`
}

// Cache is the on-disk size of one known build or package cache.
type Cache struct {
	Name string  `json:"name"`
	Path string  `json:"path"`
	GB   float64 `json:"gb"`
}

// diskRate is one device's rates over one sampled interval. Nil fields are
// not measurable on the platform.
type diskRate struct {
	active, latencyMs, writeMBs, readMBs, throughputMBs *float64
}

// rawSample is what a platform sampler returns before summarizing.
type rawSample struct {
	cpuBusy   []float64
	memory    *Memory
	disks     []map[string]diskRate
	diskScope string
	notes     []string          // informational notes kept in the reasons
	missing   map[string]string // metric -> why it could not be sampled
}

// miss records why a metric could not be sampled; the first reason wins.
func (r *rawSample) miss(metric string, err error) {
	if r.missing == nil {
		r.missing = map[string]string{}
	}
	if _, ok := r.missing[metric]; !ok {
		r.missing[metric] = err.Error()
	}
}

func f64(v float64) *float64 { return &v }
