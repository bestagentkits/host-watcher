//go:build !windows && !linux && !darwin

package hoststatus

import (
	"context"
	"errors"
	"runtime"
	"time"
)

var errUnsupported = errors.New("host sampling is not supported on " + runtime.GOOS)

func sampleHost(context.Context, int, time.Duration) rawSample {
	var raw rawSample
	for _, m := range []string{"cpu", "memory", "disk"} {
		raw.miss(m, errUnsupported)
	}
	return raw
}

func volumeKey(string) (string, string, error) { return "", "", errUnsupported }

func diskSpace(string) (uint64, uint64, error) { return 0, 0, errUnsupported }

func fixedDrives() []string { return nil }

func readProcTable() (*procTable, error) { return nil, errUnsupported }

const procIOUnavailable = "top_io unavailable: " + "process listing is not supported on this platform"
