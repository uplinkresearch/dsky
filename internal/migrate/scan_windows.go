package migrate

import (
	"context"
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/uplinkresearch/dsky/internal/elevate"
)

// On Windows the readings come from one PowerShell script, run once, printing
// one JSON document. Windows 10 ships PowerShell 5.1 and nothing newer, and a
// scan has to work on a machine somebody is standing at with no preparation,
// so the script uses only what 5.1 has -- and DSKY's own binary needs no
// installation to carry it.
//
// Why a script rather than Go: every one of these readings is a WMI class, a
// registry hive or a cmdlet, and PowerShell reaches them in a line each. The
// Go equivalent is ten times the code for the same calls, and when a reading
// comes back strange on a customer's PC the script can be run by hand, on
// that PC, and read.

//go:embed scan.ps1
var scanFS embed.FS

// toCRLF gives the script Windows line endings without doubling any it
// already has.
func toCRLF(b []byte) []byte {
	return []byte(strings.ReplaceAll(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n", "\r\n"))
}

// ScanTimeout is the whole scan. The slow parts are the Store packages and,
// with --all-users, loading hives; both are minutes at worst on a tired
// machine, and a scan that hangs forever is worse than one that gives up.
const ScanTimeout = 15 * time.Minute

// NewCollector reads this machine. It needs administrator rights: the other
// users\' registry hives and the driver list are not readable without them,
// and a scan missing half a machine is not worth having.
func NewCollector(ctx context.Context, opts ScanOptions, usmtPath string) (Collector, error) {
	if !elevate.IsElevated() {
		return nil, fmt.Errorf("scanning reads every user's settings and the driver list, which needs administrator rights — %s", elevate.Hint())
	}
	script, err := scanFS.ReadFile("scan.ps1")
	if err != nil {
		return nil, err
	}
	// Written beside the executable's temporary folder rather than piped in:
	// a script on stdin cannot take parameters, and -EncodedCommand makes
	// what ran unreadable in a log.
	dir, err := os.MkdirTemp("", "dsky-scan")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "dsky-scan.ps1")
	// CRLF on the way out rather than in the checkout: the embedded copy is
	// the same bytes on every platform that builds DSKY (see .gitattributes),
	// and Windows gets the line endings its shell expects.
	if err := os.WriteFile(path, toCRLF(script), 0o600); err != nil {
		return nil, err
	}

	args := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", path}
	if opts.AllUsers {
		args = append(args, "-AllUsers")
	}
	if opts.ProfileSizes {
		args = append(args, "-ProfileSizes")
	}
	if usmtPath != "" {
		args = append(args, "-UsmtPath", usmtPath)
	}
	ctx, cancel := context.WithTimeout(ctx, ScanTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("the scan did not finish within %s: %s", ScanTimeout, msg)
		}
		return nil, fmt.Errorf("the scan could not run: %s", msg)
	}
	return ParseScanJSON(out)
}
