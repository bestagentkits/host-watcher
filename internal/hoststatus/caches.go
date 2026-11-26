package hoststatus

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type cacheLocation struct{ name, path string }

// cacheReport sizes the known build and package caches that exist. Walks do
// not follow symlinks; unreadable entries are skipped.
func cacheReport(ctx context.Context) []Cache {
	out := []Cache{}
	for _, loc := range cacheLocations(ctx) {
		if loc.path == "" {
			continue
		}
		if info, err := os.Stat(loc.path); err != nil || !info.IsDir() {
			continue
		}
		size := treeSize(ctx, loc.path)
		out = append(out, Cache{Name: loc.name, Path: loc.path, GB: round(float64(size)/bytesPerGiB, 2)})
	}
	return out
}

func treeSize(ctx context.Context, root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

func cacheLocations(ctx context.Context) []cacheLocation {
	home, _ := os.UserHomeDir()
	userCache, _ := os.UserCacheDir()
	goEnv := goEnvValues(ctx, "GOCACHE", "GOMODCACHE")

	goCache := goEnv["GOCACHE"]
	if goCache == "" && userCache != "" {
		goCache = filepath.Join(userCache, "go-build")
	}
	goMod := goEnv["GOMODCACHE"]
	if goMod == "" {
		gopath := strings.TrimSpace(os.Getenv("GOPATH"))
		if gopath == "" && home != "" {
			gopath = filepath.Join(home, "go")
		}
		if gopath != "" {
			goMod = filepath.Join(filepath.SplitList(gopath)[0], "pkg", "mod")
		}
	}
	golangci := envOr("GOLANGCI_LINT_CACHE", joinIf(userCache, "golangci-lint"))

	var pnpm, npm string
	switch runtime.GOOS {
	case "windows":
		local := envOr("LOCALAPPDATA", joinIf(home, "AppData", "Local"))
		pnpm, npm = joinIf(local, "pnpm", "store"), joinIf(local, "npm-cache")
	case "darwin":
		pnpm, npm = joinIf(home, "Library", "pnpm", "store"), joinIf(home, ".npm")
	default:
		data := envOr("XDG_DATA_HOME", joinIf(home, ".local", "share"))
		pnpm, npm = joinIf(data, "pnpm", "store"), joinIf(home, ".npm")
	}
	return []cacheLocation{
		{"go-build", goCache},
		{"go-mod", goMod},
		{"golangci-lint", golangci},
		{"pnpm-store", envOr("PNPM_STORE_DIR", pnpm)},
		{"npm-cache", envOr("npm_config_cache", npm)},
	}
}

// goEnvValues resolves Go settings from the environment, then from `go env`
// when Go is installed. Missing values stay empty.
func goEnvValues(ctx context.Context, keys ...string) map[string]string {
	out := map[string]string{}
	var missing []string
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" && v != "off" {
			out[k] = v
		} else {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return out
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		return out
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(cctx, goBin, append([]string{"env"}, missing...)...).Output() //nolint:gosec // go from PATH with fixed env key arguments
	if err != nil {
		// Go installed but unusable: fall back to the default locations.
		return out
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	for i, k := range missing {
		if i < len(lines) {
			if v := strings.TrimSpace(lines[i]); v != "" && v != "off" {
				out[k] = v
			}
		}
	}
	return out
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func joinIf(base string, elems ...string) string {
	if base == "" {
		return ""
	}
	return filepath.Join(append([]string{base}, elems...)...)
}

// CacheReport sizes the known build and package caches that exist.
func CacheReport(ctx context.Context) []Cache { return cacheReport(ctx) }
