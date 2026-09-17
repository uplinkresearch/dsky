//go:build !windows

package migrate

import (
	"context"
	"fmt"
	"runtime"
)

// Scanning reads a Windows machine's own registry and WMI, so it happens on
// that machine. A manifest is read, reviewed, reported and built from
// anywhere -- which is the point of it being a file.
func NewCollector(context.Context, ScanOptions, string) (Collector, error) {
	return nil, fmt.Errorf("a scan runs on the Windows PC being replaced; this is %s. Copy dsky-agent.exe to that machine (or a stick) and run `dsky-agent scan`", runtime.GOOS)
}
