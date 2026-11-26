package hoststatus

import (
	"bufio"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// cpuTimes is aggregate CPU time in arbitrary but consistent units.
type cpuTimes struct{ idle, total uint64 }

// busyPct returns the busy share between two readings, or false when the
// counters did not advance.
func busyPct(prev, cur cpuTimes) (float64, bool) {
	if cur.total <= prev.total || cur.idle < prev.idle {
		return 0, false
	}
	dt := float64(cur.total - prev.total)
	di := float64(cur.idle - prev.idle)
	return clampPct((1 - di/dt) * 100), true
}

func clampPct(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	default:
		return v
	}
}

// parseProcStat reads the aggregate "cpu" line of /proc/stat. Idle time is
// idle+iowait; guest time is already counted in user and nice, so only the
// first eight fields are summed.
func parseProcStat(text string) (cpuTimes, error) {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var vals []uint64
		for _, s := range f[1:min(len(f), 9)] {
			v, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return cpuTimes{}, fmt.Errorf("parse /proc/stat cpu field %q: %w", s, err)
			}
			vals = append(vals, v)
		}
		var t cpuTimes
		for i, v := range vals {
			t.total += v
			if i == 3 || i == 4 {
				t.idle += v
			}
		}
		return t, nil
	}
	return cpuTimes{}, fmt.Errorf("parse /proc/stat: no aggregate cpu line")
}

// parseMeminfo reads MemTotal and MemAvailable (kB) from /proc/meminfo.
func parseMeminfo(text string) (*Memory, error) {
	vals := map[string]uint64{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok || (key != "MemTotal" && key != "MemAvailable") {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse /proc/meminfo %s: %w", key, err)
		}
		vals[key] = v * 1024
	}
	total, okT := vals["MemTotal"]
	avail, okA := vals["MemAvailable"]
	if !okT || !okA || total == 0 {
		return nil, fmt.Errorf("parse /proc/meminfo: MemTotal or MemAvailable missing")
	}
	return newMemory(total, avail), nil
}

func newMemory(totalBytes, availBytes uint64) *Memory {
	if availBytes > totalBytes {
		availBytes = totalBytes
	}
	return &Memory{
		UsedPct:     round(float64(totalBytes-availBytes)/float64(totalBytes)*100, 1),
		AvailableGB: round(float64(availBytes)/bytesPerGiB, 1),
		TotalGB:     round(float64(totalBytes)/bytesPerGiB, 1),
	}
}

// diskCounters are the cumulative /proc/diskstats fields we use.
type diskCounters struct {
	reads, readSectors, readMs, writes, writeSectors, writeMs, ioTicks uint64
}

// parseDiskstats returns whole-disk counters. isWholeDisk decides whether a
// name is a whole disk (on Linux: /sys/block/<name> exists); loop, ram,
// zram, optical and floppy devices are always skipped.
func parseDiskstats(text string, isWholeDisk func(string) bool) (map[string]diskCounters, error) {
	out := map[string]diskCounters{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 14 {
			continue
		}
		name := f[2]
		if hasAnyPrefix(name, "loop", "ram", "zram", "sr", "fd") || !isWholeDisk(name) {
			continue
		}
		var v [11]uint64
		for i := range v {
			n, err := strconv.ParseUint(f[3+i], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse /proc/diskstats %s field %d: %w", name, i, err)
			}
			v[i] = n
		}
		out[name] = diskCounters{
			reads: v[0], readSectors: v[2], readMs: v[3],
			writes: v[4], writeSectors: v[6], writeMs: v[7], ioTicks: v[9],
		}
	}
	return out, nil
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// diskstatsRates converts two diskstats readings elapsedMs apart into
// per-device rates. Counters that went backwards (wrap or reset) skip the
// device for that interval.
func diskstatsRates(prev, cur map[string]diskCounters, elapsedMs float64) map[string]diskRate {
	out := map[string]diskRate{}
	if elapsedMs <= 0 {
		return out
	}
	secs := elapsedMs / 1000
	for name, c := range cur {
		p, ok := prev[name]
		if !ok || c.ioTicks < p.ioTicks || c.reads < p.reads || c.writes < p.writes ||
			c.readMs < p.readMs || c.writeMs < p.writeMs ||
			c.readSectors < p.readSectors || c.writeSectors < p.writeSectors {
			continue
		}
		ios := float64((c.reads - p.reads) + (c.writes - p.writes))
		wait := float64((c.readMs - p.readMs) + (c.writeMs - p.writeMs))
		latency := 0.0
		if ios > 0 {
			latency = wait / ios
		}
		out[name] = diskRate{
			active:    f64(clampPct(float64(c.ioTicks-p.ioTicks) / elapsedMs * 100)),
			latencyMs: f64(latency),
			writeMBs:  f64(float64(c.writeSectors-p.writeSectors) * 512 / secs / bytesPerMB),
			readMBs:   f64(float64(c.readSectors-p.readSectors) * 512 / secs / bytesPerMB),
		}
	}
	return out
}

var vmStatPageSize = regexp.MustCompile(`page size of (\d+) bytes`)

// parseVMStat returns available bytes (free + inactive + speculative pages)
// from macOS vm_stat output.
func parseVMStat(text string) (uint64, error) {
	m := vmStatPageSize.FindStringSubmatch(text)
	if m == nil {
		return 0, fmt.Errorf("parse vm_stat: page size not found")
	}
	pageSize, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil || pageSize == 0 {
		return 0, fmt.Errorf("parse vm_stat: invalid page size %q", m[1])
	}
	want := map[string]bool{"Pages free": true, "Pages inactive": true, "Pages speculative": true}
	var pages uint64
	found := 0
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok || !want[strings.TrimSpace(key)] {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(rest), "."), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse vm_stat %s: %w", key, err)
		}
		pages += v
		found++
	}
	if found == 0 {
		return 0, fmt.Errorf("parse vm_stat: no free/inactive/speculative page counts")
	}
	return pages * pageSize, nil
}

// iostatSample is one interval row of macOS `iostat -d -C`.
type iostatSample struct {
	diskMBs map[string]float64
	cpuBusy *float64
}

// parseIostat parses macOS `iostat -d -C -c N -w S` output. The first data
// row is the since-boot average and is dropped. Each disk contributes three
// columns (KB/t, tps, MB/s); when a cpu header is present, the next three
// columns are us, sy, id.
func parseIostat(text string) ([]iostatSample, error) {
	var devices []string
	hasCPU := false
	var rows [][]float64
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		if _, err := strconv.ParseFloat(f[0], 64); err != nil {
			if devices == nil && !strings.Contains(f[0], "/") {
				for _, tok := range f {
					switch {
					case tok == "cpu":
						hasCPU = true
					case strings.HasPrefix(tok, "disk"):
						devices = append(devices, tok)
					}
				}
			}
			continue
		}
		row := make([]float64, 0, len(f))
		for _, tok := range f {
			v, err := strconv.ParseFloat(tok, 64)
			if err != nil {
				return nil, fmt.Errorf("parse iostat value %q: %w", tok, err)
			}
			row = append(row, v)
		}
		rows = append(rows, row)
	}
	if devices == nil && !hasCPU {
		return nil, fmt.Errorf("parse iostat: header not found")
	}
	if len(rows) < 2 {
		return nil, fmt.Errorf("parse iostat: need at least two rows, got %d", len(rows))
	}
	var out []iostatSample
	for _, row := range rows[1:] {
		s := iostatSample{diskMBs: map[string]float64{}}
		for i, dev := range devices {
			if idx := 3*i + 2; idx < len(row) {
				s.diskMBs[dev] = row[idx]
			}
		}
		if base := 3 * len(devices); hasCPU && base+2 < len(row) {
			s.cpuBusy = f64(clampPct(100 - row[base+2]))
		}
		out = append(out, s)
	}
	return out, nil
}
