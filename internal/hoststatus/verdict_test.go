package hoststatus

import (
	"strings"
	"testing"
)

func diskWith(active, latency *float64) *Disk {
	return &Disk{Device: "0 C:", ActivePct: active, LatencyMs: latency, Scope: ScopeHost}
}

func TestEvaluateVerdicts(t *testing.T) {
	calm := func() Report {
		return Report{
			CPU:    &CPU{BusyPct: 10, Cores: 8},
			Memory: &Memory{UsedPct: 40, AvailableGB: 10, TotalGB: 16},
			Disk:   diskWith(f64(5), f64(1)),
			Space:  []Space{{Path: "/", FreeGB: 100, TotalGB: 500}},
		}
	}
	tests := []struct {
		name       string
		mutate     func(*Report)
		want       Verdict
		wantReason string
	}{
		{"all calm", func(*Report) {}, VerdictGo, ""},
		{"disk active hold", func(r *Report) { r.Disk.ActivePct = f64(97) }, VerdictHold, "disk hold: active 97%"},
		{"disk latency hold", func(r *Report) { r.Disk.LatencyMs = f64(100) }, VerdictHold, "latency 100 ms"},
		{"disk active throttle", func(r *Report) { r.Disk.ActivePct = f64(70) }, VerdictThrottle, "disk throttle"},
		{"disk latency throttle", func(r *Report) { r.Disk.LatencyMs = f64(50) }, VerdictThrottle, "disk throttle"},
		{"disk latency only", func(r *Report) { r.Disk = diskWith(nil, f64(120)) }, VerdictHold, "disk hold: latency 120 ms"},
		{"cpu hold", func(r *Report) { r.CPU.BusyPct = 95 }, VerdictHold, "cpu hold"},
		{"cpu throttle", func(r *Report) { r.CPU.BusyPct = 85 }, VerdictThrottle, "cpu throttle"},
		{"cpu below throttle", func(r *Report) { r.CPU.BusyPct = 84.9 }, VerdictGo, ""},
		{"memory hold", func(r *Report) { r.Memory.UsedPct = 96 }, VerdictHold, "memory hold"},
		{"memory throttle", func(r *Report) { r.Memory.UsedPct = 88 }, VerdictThrottle, "memory throttle"},
		{"low space clean", func(r *Report) { r.Space[0].FreeGB = 24.9 }, VerdictClean, "space clean: / has 24.9 GB free"},
		{"hold beats clean", func(r *Report) { r.Space[0].FreeGB = 1; r.CPU.BusyPct = 99 }, VerdictHold, "space clean"},
		{"clean beats throttle", func(r *Report) { r.Space[0].FreeGB = 1; r.CPU.BusyPct = 90 }, VerdictClean, "cpu throttle"},
		{"all null unknown", func(r *Report) { r.CPU, r.Memory, r.Disk = nil, nil, nil }, VerdictUnknown, "no pressure metric"},
		{"throughput-only disk is not pressure", func(r *Report) {
			r.CPU, r.Memory = nil, nil
			r.Disk = &Disk{Device: "disk0", ThroughputMBs: f64(40), Scope: ScopeUnavailable}
		}, VerdictUnknown, "no pressure metric"},
		{"clean beats unknown", func(r *Report) { r.CPU, r.Memory, r.Disk = nil, nil, nil; r.Space[0].FreeGB = 3 }, VerdictClean, "space clean"},
		{"unknown beats nothing else but partial is fine", func(r *Report) { r.Disk = nil }, VerdictGo, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := calm()
			tc.mutate(&r)
			Evaluate(&r, DefaultMinFreeGB)
			if r.Verdict != tc.want {
				t.Fatalf("verdict = %s, want %s (reasons %v)", r.Verdict, tc.want, r.Reasons)
			}
			joined := strings.Join(r.Reasons, "|")
			if tc.wantReason == "" && len(r.Reasons) != 0 {
				t.Fatalf("reasons = %v, want none", r.Reasons)
			}
			if !strings.Contains(joined, tc.wantReason) {
				t.Fatalf("reasons %q missing %q", joined, tc.wantReason)
			}
			if r.Reasons == nil {
				t.Fatal("reasons must be an empty slice, not nil, so JSON renders []")
			}
		})
	}
}

func TestEvaluateMarksLowSpaceAndKeepsNotes(t *testing.T) {
	r := Report{
		Reasons: []string{"memory unavailable: boom"},
		CPU:     &CPU{BusyPct: 1},
		Space:   []Space{{Path: "a", FreeGB: 30}, {Path: "b", FreeGB: 10}},
	}
	Evaluate(&r, 25)
	if r.Space[0].Low || !r.Space[1].Low {
		t.Fatalf("low flags = %v,%v; want false,true", r.Space[0].Low, r.Space[1].Low)
	}
	if r.Reasons[0] != "memory unavailable: boom" {
		t.Fatalf("sampling note must come first, got %v", r.Reasons)
	}
	Evaluate(&r, 5)
	if r.Space[1].Low || r.Verdict != VerdictGo {
		t.Fatalf("a lower --min-free-gb must clear the flag; got low=%v verdict=%s", r.Space[1].Low, r.Verdict)
	}
}

func TestMedian(t *testing.T) {
	if _, ok := median(nil); ok {
		t.Fatal("empty median must report !ok")
	}
	if m, _ := median([]float64{3, 1, 2}); m != 2 {
		t.Fatalf("odd median = %v", m)
	}
	if m, _ := median([]float64{4, 1, 3, 2}); m != 2.5 {
		t.Fatalf("even median = %v", m)
	}
}

func TestSummarizeDisksPicksBusiestByMedian(t *testing.T) {
	samples := []map[string]diskRate{
		{"sda": {active: f64(99), latencyMs: f64(5), writeMBs: f64(1), readMBs: f64(0)},
			"sdb": {active: f64(60), latencyMs: f64(2), writeMBs: f64(3), readMBs: f64(1)}},
		{"sda": {active: f64(10), latencyMs: f64(1), writeMBs: f64(1), readMBs: f64(0)},
			"sdb": {active: f64(70), latencyMs: f64(2), writeMBs: f64(5), readMBs: f64(1)}},
		{"sda": {active: f64(12), latencyMs: f64(1), writeMBs: f64(1), readMBs: f64(0)},
			"sdb": {active: f64(65), latencyMs: f64(2), writeMBs: f64(4), readMBs: f64(1)}},
	}
	d := summarizeDisks(samples, ScopeWSLGuest)
	if d == nil || d.Device != "sdb" {
		t.Fatalf("busiest = %+v, want sdb (one spike on sda must not win the median)", d)
	}
	if *d.ActivePct != 65 || *d.WriteMBs != 4 || *d.ThroughputMBs != 5 || d.Scope != ScopeWSLGuest {
		t.Fatalf("summary = active %v write %v total %v scope %s", *d.ActivePct, *d.WriteMBs, *d.ThroughputMBs, d.Scope)
	}
	if summarizeDisks(nil, ScopeHost) != nil {
		t.Fatal("no samples must summarize to nil")
	}
}

func TestSummarizeDisksThroughputOnly(t *testing.T) {
	d := summarizeDisks([]map[string]diskRate{
		{"disk0": {throughputMBs: f64(1)}, "disk4": {throughputMBs: f64(9)}},
	}, ScopeUnavailable)
	if d.Device != "disk4" || d.ActivePct != nil || d.LatencyMs != nil || d.WriteMBs != nil || *d.ThroughputMBs != 9 {
		t.Fatalf("throughput-only summary = %+v", d)
	}
}

func TestAddCallerSpaceKeepsListedVolumesAndRaisesClean(t *testing.T) {
	dir := t.TempDir()
	r := &Report{Verdict: VerdictGo}
	AddCallerSpace(r, dir, 1e12) // every real volume is below 1e12 GB
	if len(r.Space) != 1 || !r.Space[0].Low || r.Verdict != VerdictClean {
		t.Fatalf("want one low volume and clean verdict, got %+v verdict %s", r.Space, r.Verdict)
	}
	AddCallerSpace(r, dir, 1e12)
	if len(r.Space) != 1 {
		t.Fatalf("listed volume duplicated: %+v", r.Space)
	}
	held := &Report{Verdict: VerdictHold}
	AddCallerSpace(held, dir, 1e12)
	if held.Verdict != VerdictHold {
		t.Fatalf("hold must stay more severe than clean, got %s", held.Verdict)
	}
}
