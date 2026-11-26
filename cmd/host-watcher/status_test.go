package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bestagentkits/host-watcher/internal/hoststatus"
)

func TestStatusInvalidFlagsExitTwo(t *testing.T) {
	t.Setenv("HOST_WATCHER_HOME", t.TempDir())
	for _, args := range [][]string{
		{"status", "--samples", "0"},
		{"status", "--interval", "1ms"},
		{"status", "--min-free-gb", "-1"},
		{"nope"},
	} {
		var out bytes.Buffer
		if code := run(args, nil, &out, &out); code != 2 {
			t.Fatalf("run(%v) = %d, want 2", args, code)
		}
	}
}

func TestStatusJSONEnvelopeAndWriteCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOST_WATCHER_HOME", dir)
	cache := filepath.Join(dir, "cache.json")
	var out, errb bytes.Buffer
	if code := run([]string{"status", "--json", "--fresh", "--samples", "1", "--interval", "200ms", "--path", dir,
		"--write-cache", cache}, nil, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var env struct {
		Kind string             `json:"kind"`
		Data *hoststatus.Report `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil || env.Kind != "host.status" || env.Data == nil {
		t.Fatalf("envelope %q: %v", out.String(), err)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("write-cache: %v", err)
	}
}

func TestDiskLineThroughputOnly(t *testing.T) {
	v := 3.5
	got := diskLine(&hoststatus.Disk{Device: "disk0", ThroughputMBs: &v, Scope: hoststatus.ScopeUnavailable})
	if !strings.Contains(got, "throughput 3.50 MB/s") || !strings.Contains(got, "(unavailable)") {
		t.Fatalf("diskLine = %q", got)
	}
}

func TestSubcommandHelpExitsZero(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"status", "-h"}, nil, &out, &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
}
