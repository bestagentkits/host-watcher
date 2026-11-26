package hoststatus

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Options configures one Collect run.
type Options struct {
	Samples   int
	Interval  time.Duration
	Paths     []string // empty means the default watched paths
	MinFreeGB float64
	Caches    bool
	// TopIO ranks processes by I/O over the sampling window.
	TopIO bool
	Now   func() time.Time

	// sampler overrides the platform sampler in tests.
	sampler sampler
}

// sampler collects raw per-interval CPU and disk rates plus memory. Each
// platform provides one in its build-tagged file.
type sampler interface {
	sample(ctx context.Context, n int, interval time.Duration) rawSample
}

// platformSampler samples the current OS through sampleHost.
type platformSampler struct{}

func (platformSampler) sample(ctx context.Context, n int, interval time.Duration) rawSample {
	return sampleHost(ctx, n, interval)
}

// DefaultCachePath is where `--write-cache` consumers look by default.
func DefaultCachePath() string {
	return filepath.Join(os.TempDir(), DefaultCacheFileName)
}

// Collect samples the host and returns an evaluated report. It only fails
// for invalid options or a cancelled context; unavailable metrics become
// null fields with a reason.
func Collect(ctx context.Context, opts Options) (*Report, error) {
	if opts.Samples < 1 {
		return nil, fmt.Errorf("samples must be at least 1, got %d", opts.Samples)
	}
	if opts.Interval <= 0 {
		return nil, fmt.Errorf("interval must be positive, got %s", opts.Interval)
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}

	var smp sampler = platformSampler{}
	if opts.sampler != nil {
		smp = opts.sampler
	}
	var before *procTable
	var procErr error
	start := time.Now()
	if opts.TopIO {
		before, procErr = readProcTable()
	}
	raw := smp.sample(ctx, opts.Samples, opts.Interval)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var top []ProcIO
	if opts.TopIO {
		top, procErr = sampleTopIO(before, procErr, time.Since(start))
	}
	r := &Report{
		SampledAt: now().UTC().Format(time.RFC3339),
		Source:    SourceSampled,
		OS:        runtime.GOOS,
		Memory:    raw.memory,
		TopIO:     top,
		IOScope:   topIOScope(runtime.GOOS, len(top) > 0),
		Reasons:   append([]string(nil), raw.notes...),
	}
	if busy, ok := median(raw.cpuBusy); ok {
		r.CPU = &CPU{BusyPct: round(busy, 1), Cores: runtime.NumCPU()}
	}
	r.Disk = summarizeDisks(raw.disks, raw.diskScope)
	noteMissing(r, raw, "cpu", r.CPU == nil)
	noteMissing(r, raw, "memory", r.Memory == nil)
	noteMissing(r, raw, "disk", r.Disk == nil)

	paths := opts.Paths
	if len(paths) == 0 {
		paths = defaultPaths()
	}
	var spaceNotes []string
	r.Space, spaceNotes = spaceReport(paths)
	r.Reasons = append(r.Reasons, spaceNotes...)
	if procErr != nil {
		r.Reasons = append(r.Reasons, procErr.Error())
	}
	if opts.Caches {
		r.Caches = cacheReport(ctx)
	}
	Evaluate(r, opts.MinFreeGB)
	return r, nil
}

// noteMissing gives every null metric a reason: the sampler's own, or
// "no samples" when it produced none (for example no counter advanced).
func noteMissing(r *Report, raw rawSample, metric string, missing bool) {
	if !missing {
		return
	}
	why, ok := raw.missing[metric]
	if !ok {
		why = "no samples"
	}
	r.Reasons = append(r.Reasons, metric+" unavailable: "+why)
}

// defaultPaths is cwd, home, temp, plus every fixed drive on Windows.
func defaultPaths() []string {
	var paths []string
	if wd, err := os.Getwd(); err == nil {
		paths = append(paths, wd)
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, home)
	}
	paths = append(paths, os.TempDir())
	return append(paths, fixedDrives()...)
}

// spaceReport returns one entry per distinct volume among paths, plus a
// note for each path that could not be read.
func spaceReport(paths []string) ([]Space, []string) {
	seen := map[string]bool{}
	out := []Space{}
	var notes []string
	for _, p := range paths {
		key, label, err := volumeKey(p)
		if err != nil {
			notes = append(notes, fmt.Sprintf("space unavailable for %s: %v", p, err))
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		free, total, err := diskSpace(p)
		if err != nil {
			notes = append(notes, fmt.Sprintf("space unavailable for %s: %v", p, err))
			continue
		}
		out = append(out, Space{
			Path:    label,
			FreeGB:  round(float64(free)/bytesPerGiB, 1),
			TotalGB: round(float64(total)/bytesPerGiB, 1),
		})
	}
	return out, notes
}

// AddCallerSpace adds the volume holding dir to a report built elsewhere
// (the watcher's snapshot), so a caller on a volume the watcher does not
// watch still gets its low-space check. A volume already listed is left as is.
func AddCallerSpace(r *Report, dir string, minFreeGB float64) {
	extra, _ := spaceReport([]string{dir})
	for _, s := range extra {
		dup := false
		for _, have := range r.Space {
			if have.Path == s.Path {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		s.Low = s.FreeGB < minFreeGB
		r.Space = append(r.Space, s)
		if s.Low {
			r.Reasons = append(r.Reasons, fmt.Sprintf("space clean: %s has %.1f GB free (< %g GB)", s.Path, s.FreeGB, minFreeGB))
			if VerdictClean.severity() > r.Verdict.severity() {
				r.Verdict = VerdictClean
			}
		}
	}
}

// sleepCtx waits d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// sampleTopIO takes the second process snapshot and ranks the I/O between
// the two. It returns nil plus a reason when the platform or the snapshots
// cannot provide per-process rates.
func sampleTopIO(before *procTable, beforeErr error, elapsed time.Duration) ([]ProcIO, error) {
	if procIOUnavailable != "" {
		return nil, errors.New(procIOUnavailable)
	}
	if beforeErr != nil {
		return nil, fmt.Errorf("top_io unavailable: %w", beforeErr)
	}
	after, err := readProcTable()
	if err != nil {
		return nil, fmt.Errorf("top_io unavailable: %w", err)
	}
	exists := cachedExists(func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	})
	return topIO(before, after, elapsed.Seconds(), TopIOLimit, exists), nil
}

// AgentRuntimeAlive reports whether any agent runtime process (Claude Code,
// Codex, OpenCode, Pi, Cursor, or a script host running one) is alive.
func AgentRuntimeAlive() (bool, error) {
	t, err := readProcTable()
	if err != nil {
		return false, err
	}
	return t.agentAlive(), nil
}
