//go:build darwin

package hoststatus

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// sampleHost uses one `iostat -d -C` run for CPU busy time and per-disk
// throughput, and sysctl hw.memsize plus vm_stat for memory. macOS exposes
// no per-disk active time or latency without cgo, so the disk scope is
// unavailable and only throughput is reported.
func sampleHost(ctx context.Context, n int, interval time.Duration) rawSample {
	raw := rawSample{diskScope: ScopeUnavailable}
	if mem, err := darwinMemory(ctx); err != nil {
		raw.miss("memory", err)
	} else {
		raw.memory = mem
	}

	secs := int(math.Ceil(interval.Seconds()))
	if secs < 1 {
		secs = 1
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(n*secs+30)*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "/usr/sbin/iostat", "-d", "-C", //nolint:gosec // fixed binary, numeric arguments
		"-c", strconv.Itoa(n+1), "-w", strconv.Itoa(secs)).Output()
	if err != nil {
		err = fmt.Errorf("iostat: %w", err)
		raw.miss("cpu", err)
		raw.miss("disk", err)
		return raw
	}
	rows, err := parseIostat(string(out))
	if err != nil {
		raw.miss("cpu", err)
		raw.miss("disk", err)
		return raw
	}
	for _, row := range rows {
		if row.cpuBusy != nil {
			raw.cpuBusy = append(raw.cpuBusy, *row.cpuBusy)
		}
		sample := map[string]diskRate{}
		for dev, mbs := range row.diskMBs {
			sample[dev] = diskRate{throughputMBs: f64(mbs)}
		}
		raw.disks = append(raw.disks, sample)
	}
	raw.notes = append(raw.notes, "disk active time and latency are not measurable on macOS without cgo; throughput only")
	return raw
}

func darwinMemory(ctx context.Context) (*Memory, error) {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return nil, fmt.Errorf("sysctl hw.memsize: %w", err)
	}
	if total == 0 {
		return nil, fmt.Errorf("sysctl hw.memsize returned 0")
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "/usr/bin/vm_stat").Output()
	if err != nil {
		return nil, fmt.Errorf("vm_stat: %w", err)
	}
	avail, err := parseVMStat(string(out))
	if err != nil {
		return nil, err
	}
	return newMemory(total, avail), nil
}
