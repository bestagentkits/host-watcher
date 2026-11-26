package hoststatus

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ConfigPath is $HOST_WATCHER_CONFIG, else <state dir>/config.json.
func ConfigPath() string {
	if p := os.Getenv("HOST_WATCHER_CONFIG"); p != "" {
		return p
	}
	p, err := DefaultPaths()
	if err != nil {
		return ""
	}
	return filepath.Join(p.Dir, "config.json")
}

// LoadConfig overrides the thresholds from a JSON file shared with the
// Python scripts: either {"thresholds": {...}} or a flat object, with keys
// such as cpu_hold_pct or min_free_gb. A missing file is not an error;
// unknown keys and non-numeric values are ignored.
func LoadConfig(path string) error {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // user-chosen config file
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("invalid config %s: %w", path, err)
	}
	if inner, ok := doc["thresholds"]; ok {
		doc = nil
		if err := json.Unmarshal(inner, &doc); err != nil {
			return fmt.Errorf("invalid config %s: thresholds: %w", path, err)
		}
	}
	targets := map[string]*float64{
		"disk_hold_active_pct": &DiskHoldActivePct, "disk_hold_latency_ms": &DiskHoldLatencyMs,
		"disk_throttle_active_pct": &DiskThrottleActivePct, "disk_throttle_latency_ms": &DiskThrottleLatencyMs,
		"cpu_hold_pct": &CPUHoldPct, "cpu_throttle_pct": &CPUThrottlePct,
		"memory_hold_pct": &MemoryHoldPct, "memory_throttle_pct": &MemoryThrottlePct,
		"min_free_gb": &DefaultMinFreeGB,
	}
	for key, raw := range doc {
		dst, ok := targets[key]
		if !ok {
			continue
		}
		var v float64
		if json.Unmarshal(raw, &v) == nil {
			*dst = v
		}
	}
	return nil
}
