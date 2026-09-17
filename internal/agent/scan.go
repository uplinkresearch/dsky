package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/uplinkresearch/dsky/internal/buildinfo"
	"github.com/uplinkresearch/dsky/internal/migrate"
)

// Scanning a PC that is about to be replaced happens from this binary,
// because this is the binary that gets carried to it: one file on a stick, no
// installation, nothing left behind. The work itself is internal/migrate, the
// same code the operator's DSKY runs, so the path used on a customer's desk
// is not a second, less-travelled one.
//
// Where the two files go: beside this executable by default, which on a stick
// is the stick. A read-only stick, or one plugged into a machine whose user
// cannot write to it, is the reason --out exists.
func scanThisMachine(args []string) error {
	dir := ""
	opts := migrate.ScanOptions{LocalAdmin: "uplink"}
	usmt := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return args[i], nil
		}
		var err error
		switch {
		case a == "--all-users":
			opts.AllUsers = true
		case a == "--profile-sizes":
			opts.ProfileSizes = true
		case a == "--out":
			dir, err = next()
		case a == "--usmt":
			usmt, err = next()
		case a == "--hostname":
			opts.Hostname, err = next()
		case strings.HasPrefix(a, "--"):
			err = fmt.Errorf("unknown option %q (--out, --all-users, --profile-sizes, --usmt, --hostname)", a)
		case dir == "":
			dir = a
		default:
			err = fmt.Errorf("unexpected %q", a)
		}
		if err != nil {
			return err
		}
	}
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		dir = filepath.Dir(exe)
	}
	res, err := migrate.ScanToFiles(context.Background(), dir, opts, usmt, "dsky-scan "+buildinfo.Version, os.Stdout)
	if err != nil {
		return err
	}
	// The same exit code the operator's DSKY uses: a machine that answered
	// everything is 0, one that held something back is 3, and a scan that
	// could not run at all is 1 with a sentence saying why.
	if len(res.Unread) > 0 {
		os.Exit(3)
	}
	return nil
}
