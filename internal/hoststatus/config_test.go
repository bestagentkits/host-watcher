package hoststatus

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigOverridesThresholds(t *testing.T) {
	saved := CPUThrottlePct
	savedFree := DefaultMinFreeGB
	t.Cleanup(func() { CPUThrottlePct, DefaultMinFreeGB = saved, savedFree })
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(`{"thresholds":{"cpu_throttle_pct":50,"min_free_gb":"x","bogus":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := LoadConfig(p); err != nil {
		t.Fatal(err)
	}
	if CPUThrottlePct != 50 || DefaultMinFreeGB != savedFree {
		t.Fatalf("cpu throttle %v, min free %v", CPUThrottlePct, DefaultMinFreeGB)
	}
	if err := LoadConfig(filepath.Join(dir, "missing.json")); err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if err := os.WriteFile(p, []byte(`{broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := LoadConfig(p); err == nil {
		t.Fatal("want error for broken config")
	}
}
