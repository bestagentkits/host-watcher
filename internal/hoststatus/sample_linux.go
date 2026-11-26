//go:build linux

package hoststatus

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"
)

func readProcFile(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec,forbidigo // fixed /proc and /sys paths, not kit content
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func isWholeDisk(name string) bool {
	_, err := os.Stat("/sys/block/" + name)
	return err == nil
}

func isWSL() bool {
	v, err := readProcFile("/proc/version")
	return err == nil && strings.Contains(strings.ToLower(v), "microsoft")
}

func readCPU() (cpuTimes, error) {
	text, err := readProcFile("/proc/stat")
	if err != nil {
		return cpuTimes{}, err
	}
	return parseProcStat(text)
}

func readDisks() (map[string]diskCounters, error) {
	text, err := readProcFile("/proc/diskstats")
	if err != nil {
		return nil, err
	}
	return parseDiskstats(text, isWholeDisk)
}

// sampleHost reads /proc/stat and /proc/diskstats at each interval boundary
// and /proc/meminfo once.
func sampleHost(ctx context.Context, n int, interval time.Duration) rawSample {
	raw := rawSample{diskScope: ScopeHost}
	if isWSL() {
		raw.diskScope = ScopeWSLGuest
	}
	if text, err := readProcFile("/proc/meminfo"); err != nil {
		raw.miss("memory", err)
	} else if mem, err := parseMeminfo(text); err != nil {
		raw.miss("memory", err)
	} else {
		raw.memory = mem
	}

	prevCPU, cpuErr := readCPU()
	prevDisk, diskErr := readDisks()
	prevAt := time.Now()
	for i := 0; i < n && (cpuErr == nil || diskErr == nil); i++ {
		if sleepCtx(ctx, interval) != nil {
			return raw
		}
		at := time.Now()
		if cpuErr == nil {
			var cur cpuTimes
			if cur, cpuErr = readCPU(); cpuErr == nil {
				if busy, ok := busyPct(prevCPU, cur); ok {
					raw.cpuBusy = append(raw.cpuBusy, busy)
				}
				prevCPU = cur
			}
		}
		if diskErr == nil {
			var cur map[string]diskCounters
			if cur, diskErr = readDisks(); diskErr == nil {
				raw.disks = append(raw.disks, diskstatsRates(prevDisk, cur, float64(at.Sub(prevAt).Microseconds())/1000))
				prevDisk = cur
			}
		}
		prevAt = at
	}
	if cpuErr != nil {
		raw.miss("cpu", cpuErr)
	}
	if diskErr != nil {
		raw.miss("disk", diskErr)
	} else if len(prevDisk) == 0 {
		raw.miss("disk", errors.New("no whole disks in /proc/diskstats"))
	}
	return raw
}
