package hoststatus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCollectRejectsInvalidOptions(t *testing.T) {
	if _, err := Collect(context.Background(), Options{Samples: 0, Interval: time.Second}); err == nil {
		t.Fatal("zero samples must error")
	}
	if _, err := Collect(context.Background(), Options{Samples: 1}); err == nil {
		t.Fatal("zero interval must error")
	}
}

func TestCollectSamplesThisHost(t *testing.T) {
	dir := t.TempDir()
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	r, err := Collect(context.Background(), Options{
		Samples: 1, Interval: 100 * time.Millisecond, Paths: []string{dir, dir},
		MinFreeGB: 0, Now: func() time.Time { return fixed },
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.OS != runtime.GOOS || r.SampledAt != "2026-01-02T03:04:05Z" || r.Verdict == "" {
		t.Fatalf("report header = %+v", r)
	}
	if len(r.Space) != 1 {
		t.Fatalf("one path given twice must dedupe to one volume, got %+v", r.Space)
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "linux" {
		if r.CPU == nil || r.Memory == nil {
			t.Fatalf("cpu/memory must sample on %s; reasons %v", runtime.GOOS, r.Reasons)
		}
	}
}

func TestCollectCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Collect(ctx, Options{Samples: 1, Interval: time.Second}); err == nil {
		t.Fatal("cancelled context must error")
	}
}

func TestSpaceReportNotesUnreadablePath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist", "x")
	spaces, notes := spaceReport([]string{missing})
	if len(spaces) != 0 || len(notes) != 1 {
		t.Fatalf("spaces=%v notes=%v", spaces, notes)
	}
}

func TestTreeSizeSumsRegularFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o750); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int{"one": 10, filepath.Join("a", "two"): 20, filepath.Join("a", "b", "three"): 30} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := treeSize(context.Background(), dir); got != 60 {
		t.Fatalf("treeSize = %d, want 60", got)
	}
}

func TestCacheLocationsHonorEnvOverrides(t *testing.T) {
	t.Setenv("GOCACHE", "/x/gocache")
	t.Setenv("GOMODCACHE", "/x/gomod")
	t.Setenv("GOLANGCI_LINT_CACHE", "/x/lint")
	t.Setenv("PNPM_STORE_DIR", "/x/pnpm")
	t.Setenv("npm_config_cache", "/x/npm")
	got := map[string]string{}
	for _, loc := range cacheLocations(context.Background()) {
		got[loc.name] = loc.path
	}
	want := map[string]string{"go-build": "/x/gocache", "go-mod": "/x/gomod", "golangci-lint": "/x/lint", "pnpm-store": "/x/pnpm", "npm-cache": "/x/npm"}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q", k, got[k], v)
		}
	}
}

type fakeSampler struct{ raw rawSample }

func (f fakeSampler) sample(context.Context, int, time.Duration) rawSample { return f.raw }

func collectFake(t *testing.T, raw rawSample) *Report {
	t.Helper()
	r, err := Collect(context.Background(), Options{
		Samples: 3, Interval: time.Second, Paths: []string{t.TempDir()}, sampler: fakeSampler{raw},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCollectWithFakeSamplesUsesMedians(t *testing.T) {
	r := collectFake(t, rawSample{
		cpuBusy:   []float64{99, 50, 52},
		memory:    &Memory{UsedPct: 60, AvailableGB: 4, TotalGB: 10},
		diskScope: ScopeHost,
		disks: []map[string]diskRate{
			{"0 C:": {active: f64(95), latencyMs: f64(120)}},
			{"0 C:": {active: f64(96), latencyMs: f64(110)}},
			{"0 C:": {active: f64(10), latencyMs: f64(1)}},
		},
	})
	if r.CPU == nil || r.CPU.BusyPct != 52 {
		t.Fatalf("cpu = %+v, want median 52", r.CPU)
	}
	if r.Verdict != VerdictHold || *r.Disk.ActivePct != 95 || *r.Disk.LatencyMs != 110 {
		t.Fatalf("verdict %s disk %+v reasons %v", r.Verdict, r.Disk, r.Reasons)
	}
}

func TestCollectNullMetricsAlwaysCarryReasons(t *testing.T) {
	missing := rawSample{}
	missing.miss("memory", errors.New("boom"))
	r := collectFake(t, missing)
	if r.CPU != nil || r.Memory != nil || r.Disk != nil || r.Verdict != VerdictUnknown {
		t.Fatalf("report = %+v", r)
	}
	joined := strings.Join(r.Reasons, "|")
	for _, want := range []string{"cpu unavailable: no samples", "memory unavailable: boom", "disk unavailable: no samples"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("reasons %q missing %q", joined, want)
		}
	}
}
