package hoststatus

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Evaluate marks low-space volumes and sets the report's verdict and
// reasons. The verdict is the most severe of the per-metric levels; when no
// pressure metric (CPU, memory, disk activity) could be sampled and space
// is fine, the verdict is unknown. Notes from sampling are kept first.
func Evaluate(r *Report, minFreeGB float64) {
	verdict := VerdictGo
	raise := func(v Verdict) {
		if v.severity() > verdict.severity() {
			verdict = v
		}
	}
	reasons := append([]string(nil), r.Reasons...)
	sampled := false

	if r.Disk != nil && (r.Disk.ActivePct != nil || r.Disk.LatencyMs != nil) {
		sampled = true
		active, latency := deref(r.Disk.ActivePct), deref(r.Disk.LatencyMs)
		level := VerdictGo
		switch {
		case active >= DiskHoldActivePct || latency >= DiskHoldLatencyMs:
			level = VerdictHold
		case active >= DiskThrottleActivePct || latency >= DiskThrottleLatencyMs:
			level = VerdictThrottle
		}
		if level != VerdictGo {
			reasons = append(reasons, fmt.Sprintf("disk %s: %s", level, diskSummary(r.Disk)))
		}
		raise(level)
	}
	if r.CPU != nil {
		sampled = true
		level := thresholdLevel(r.CPU.BusyPct, CPUHoldPct, CPUThrottlePct)
		if level != VerdictGo {
			reasons = append(reasons, fmt.Sprintf("cpu %s: busy %.0f%%", level, r.CPU.BusyPct))
		}
		raise(level)
	}
	if r.Memory != nil {
		sampled = true
		level := thresholdLevel(r.Memory.UsedPct, MemoryHoldPct, MemoryThrottlePct)
		if level != VerdictGo {
			reasons = append(reasons, fmt.Sprintf("memory %s: used %.0f%%", level, r.Memory.UsedPct))
		}
		raise(level)
	}
	for i := range r.Space {
		s := &r.Space[i]
		s.Low = s.FreeGB < minFreeGB
		if s.Low {
			reasons = append(reasons, fmt.Sprintf("space clean: %s has %.1f GB free (< %g GB)", s.Path, s.FreeGB, minFreeGB))
			raise(VerdictClean)
		}
	}
	if !sampled {
		reasons = append(reasons, "no pressure metric could be sampled")
		raise(VerdictUnknown)
	}
	r.Verdict = verdict
	if reasons == nil {
		reasons = []string{}
	}
	r.Reasons = reasons
}

func thresholdLevel(v, hold, throttle float64) Verdict {
	switch {
	case v >= hold:
		return VerdictHold
	case v >= throttle:
		return VerdictThrottle
	default:
		return VerdictGo
	}
}

func diskSummary(d *Disk) string {
	var parts []string
	if d.ActivePct != nil {
		parts = append(parts, fmt.Sprintf("active %.0f%%", *d.ActivePct))
	}
	if d.LatencyMs != nil {
		parts = append(parts, fmt.Sprintf("latency %.0f ms", *d.LatencyMs))
	}
	return strings.Join(parts, " ")
}

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// median returns the median of values; ok is false for an empty slice.
func median(values []float64) (float64, bool) {
	if len(values) == 0 {
		return 0, false
	}
	s := append([]float64(nil), values...)
	sort.Float64s(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid], true
	}
	return (s[mid-1] + s[mid]) / 2, true
}

func round(v float64, digits int) float64 {
	p := math.Pow(10, float64(digits))
	return math.Round(v*p) / p
}

// medianField returns the rounded median of one field across samples, or
// nil when no sample measured it.
func medianField(rates []diskRate, pick func(diskRate) *float64, digits int) *float64 {
	var vals []float64
	for _, r := range rates {
		if p := pick(r); p != nil {
			vals = append(vals, *p)
		}
	}
	m, ok := median(vals)
	if !ok {
		return nil
	}
	return f64(round(m, digits))
}

// summarizeDisks takes per-interval device rates and returns the busiest
// device by median active time, then latency, then throughput. It returns
// nil when no device was sampled.
func summarizeDisks(samples []map[string]diskRate, scope string) *Disk {
	byDevice := map[string][]diskRate{}
	for _, s := range samples {
		for name, rate := range s {
			byDevice[name] = append(byDevice[name], rate)
		}
	}
	names := make([]string, 0, len(byDevice))
	for name := range byDevice {
		names = append(names, name)
	}
	sort.Strings(names)
	var best *Disk
	for _, name := range names {
		rates := byDevice[name]
		d := &Disk{
			Device:        name,
			ActivePct:     medianField(rates, func(r diskRate) *float64 { return r.active }, 1),
			LatencyMs:     medianField(rates, func(r diskRate) *float64 { return r.latencyMs }, 1),
			WriteMBs:      medianField(rates, func(r diskRate) *float64 { return r.writeMBs }, 2),
			ReadMBs:       medianField(rates, func(r diskRate) *float64 { return r.readMBs }, 2),
			ThroughputMBs: medianField(rates, func(r diskRate) *float64 { return r.throughputMBs }, 2),
			Scope:         scope,
		}
		if d.ThroughputMBs == nil && d.WriteMBs != nil && d.ReadMBs != nil {
			d.ThroughputMBs = f64(round(*d.WriteMBs+*d.ReadMBs, 2))
		}
		if best == nil || busier(d, best) {
			best = d
		}
	}
	return best
}

func busier(a, b *Disk) bool {
	keys := func(d *Disk) [3]float64 {
		return [3]float64{deref(d.ActivePct), deref(d.LatencyMs), deref(d.ThroughputMBs)}
	}
	ka, kb := keys(a), keys(b)
	for i := range ka {
		if ka[i] != kb[i] {
			return ka[i] > kb[i]
		}
	}
	return false
}
