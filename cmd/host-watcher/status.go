package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/bestagentkits/host-watcher/internal/hoststatus"
)

const (
	maxSamples  = 30
	minInterval = 100 * time.Millisecond
	maxInterval = time.Minute
)

type statusOptions struct {
	samples    int
	interval   time.Duration
	caches     bool
	paths      stringList
	minFreeGB  float64
	writeCache string
	fresh      bool
	json       bool
}

func (o statusOptions) validate() error {
	switch {
	case o.samples < 1 || o.samples > maxSamples:
		return fmt.Errorf("--samples must be between 1 and %d, got %d", maxSamples, o.samples)
	case o.interval < minInterval || o.interval > maxInterval:
		return fmt.Errorf("--interval must be between %s and %s, got %s", minInterval, maxInterval, o.interval)
	case o.minFreeGB < 0:
		return fmt.Errorf("--min-free-gb must not be negative, got %g", o.minFreeGB)
	}
	return nil
}

func cmdStatus(ctx context.Context, args []string, stdout io.Writer) error {
	o := statusOptions{samples: hoststatus.DefaultSamples, interval: hoststatus.DefaultInterval, minFreeGB: hoststatus.DefaultMinFreeGB}
	fs := newFlags("status")
	fs.IntVar(&o.samples, "samples", o.samples, "number of sampling intervals (1-30)")
	fs.DurationVar(&o.interval, "interval", o.interval, "length of each sampling interval (100ms-1m)")
	fs.BoolVar(&o.caches, "caches", false, "also report the size of known build and package caches")
	fs.Var(&o.paths, "path", "volume to watch for free space (repeatable; replaces the defaults)")
	fs.Float64Var(&o.minFreeGB, "min-free-gb", o.minFreeGB, "free space in GB below which the verdict is clean")
	fs.BoolVar(&o.fresh, "fresh", false, "sample now even when the watcher's snapshot is fresh")
	fs.StringVar(&o.writeCache, "write-cache", "", "also write the JSON envelope atomically to this file")
	fs.BoolVar(&o.json, "json", false, "print the JSON envelope")
	if err := fs.Parse(args); err != nil {
		return exitErr(2, err)
	}
	if err := o.validate(); err != nil {
		return exitErr(2, err)
	}
	r, err := currentStatus(ctx, o)
	if err != nil {
		return exitErr(1, err)
	}
	if o.writeCache != "" {
		b, err := hoststatus.EncodeSnapshot(r)
		if err == nil {
			err = writeFileAtomic(o.writeCache, b)
		}
		if err != nil {
			return exitErr(1, fmt.Errorf("write --write-cache %s: %w", o.writeCache, err))
		}
	}
	if o.json {
		return writeJSON(stdout, hoststatus.SnapshotKind, r)
	}
	return renderStatus(stdout, r)
}

// currentStatus returns the watcher's snapshot when it is fresh and the
// caller asked for nothing it does not carry; otherwise it samples now. The
// caller worktree's inbox entry is attached without being consumed.
func currentStatus(ctx context.Context, o statusOptions) (*hoststatus.Report, error) {
	paths, pathErr := hoststatus.DefaultPaths()
	now := time.Now()
	var report *hoststatus.Report
	if pathErr == nil && !o.fresh && !o.caches && len(o.paths) == 0 && o.minFreeGB == hoststatus.DefaultMinFreeGB {
		report = hoststatus.ReadSnapshot(paths.Snapshot, hoststatus.SnapshotMaxAge, now)
		if report != nil {
			if wd, err := os.Getwd(); err == nil {
				hoststatus.AddCallerSpace(report, wd, o.minFreeGB)
			}
		}
	}
	if report == nil {
		var err error
		report, err = hoststatus.Collect(ctx, hoststatus.Options{
			Samples: o.samples, Interval: o.interval, Paths: o.paths, MinFreeGB: o.minFreeGB, Caches: o.caches, TopIO: true,
		})
		if err != nil {
			return nil, err
		}
	}
	if pathErr != nil {
		report.Reasons = append(report.Reasons, pathErr.Error())
		return report, nil
	}
	report.SnapshotPath = paths.Snapshot
	if wd, err := os.Getwd(); err == nil {
		_ = hoststatus.PruneInbox(paths.Inbox, now)
		report.Inbox = hoststatus.ReadInbox(paths.Inbox, hoststatus.WorktreeRoot(wd), now)
	}
	return report, nil
}

func marker(v hoststatus.Verdict) string {
	switch v {
	case hoststatus.VerdictGo:
		return "[ok]"
	case hoststatus.VerdictHold:
		return "[!!]"
	default:
		return "[! ]"
	}
}

func renderStatus(w io.Writer, r *hoststatus.Report) error {
	lines := []string{fmt.Sprintf("%s verdict %s (%s, %s at %s)", marker(r.Verdict), r.Verdict, r.OS, r.Source, r.SampledAt)}
	if r.CPU != nil {
		lines = append(lines, fmt.Sprintf("cpu     %.1f%% busy, %d cores", r.CPU.BusyPct, r.CPU.Cores))
	} else {
		lines = append(lines, "cpu     unavailable")
	}
	if r.Memory != nil {
		lines = append(lines, fmt.Sprintf("memory  %.1f%% used, %.1f GB available of %.1f GB", r.Memory.UsedPct, r.Memory.AvailableGB, r.Memory.TotalGB))
	} else {
		lines = append(lines, "memory  unavailable")
	}
	if r.Disk != nil {
		lines = append(lines, "disk    "+diskLine(r.Disk))
	} else {
		lines = append(lines, "disk    unavailable")
	}
	for _, sp := range r.Space {
		low := ""
		if sp.Low {
			low = "  LOW"
		}
		lines = append(lines, fmt.Sprintf("space   %s %.1f GB free of %.1f GB%s", sp.Path, sp.FreeGB, sp.TotalGB, low))
	}
	for _, p := range r.TopIO {
		where := p.Worktree
		if where == "" {
			where = p.Cwd
		}
		lines = append(lines, fmt.Sprintf("io      %s pid %d write %.2f MB/s read %.2f MB/s %s %s", p.Name, p.PID, p.WriteMBs, p.ReadMBs, p.Runtime, where))
	}
	if r.IOScope != "" && len(r.TopIO) > 0 {
		lines = append(lines, "io      rates are "+r.IOScope)
	}
	if e := r.Inbox; e != nil {
		lines = append(lines, fmt.Sprintf("[! ] inbox %s (%s): %s", e.Verdict, e.SampledAt, e.Action))
	}
	for _, c := range r.Caches {
		lines = append(lines, fmt.Sprintf("cache   %s %.2f GB %s", c.Name, c.GB, c.Path))
	}
	for _, reason := range r.Reasons {
		lines = append(lines, "[i ] "+reason)
	}
	_, err := io.WriteString(w, strings.Join(lines, "\n")+"\n")
	return err
}

func diskLine(d *hoststatus.Disk) string {
	parts := []string{d.Device}
	add := func(label string, v *float64, format string) {
		if v != nil {
			parts = append(parts, label+" "+fmt.Sprintf(format, *v))
		}
	}
	add("active", d.ActivePct, "%.1f%%")
	add("latency", d.LatencyMs, "%.1f ms")
	add("write", d.WriteMBs, "%.2f MB/s")
	add("read", d.ReadMBs, "%.2f MB/s")
	if d.WriteMBs == nil {
		add("throughput", d.ThroughputMBs, "%.2f MB/s")
	}
	return strings.Join(parts, "  ") + " (" + d.Scope + ")"
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
