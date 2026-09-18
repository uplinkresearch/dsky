package migrate

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// USMT is Microsoft's User State Migration Tool, which is how a person's files
// and profile settings actually move between two machines.
//
// DSKY never ships it, and that is not caution about size. USMT comes in the
// Windows ADK under Microsoft's own licence; redistributing it is not DSKY's
// to do. So an operator installs the ADK, points at the folder, and DSKY runs
// what is already on their machine. A tool that is not there is said plainly
// before anything else happens, because the alternative is discovering it
// after the old PC has been wiped.
//
// Two commands, run by a person, on two machines: capture on the old one
// before it is retired, restore on the new one once it is built and joined.
// Neither is done by the first-boot agent, and that is deliberate. An
// encrypted store needs its key, and the one rule this feature does not bend
// is that a secret never rides on the USB. A key typed by the operator on the
// machine in front of them never goes near the media.
type USMT struct {
	// Dir holds scanstate.exe and loadstate.exe: the ADK's USMT folder for
	// this architecture.
	Dir string
	// run is swappable so the argument-building can be tested without the ADK.
	run func(ctx context.Context, name string, args ...string) (int, string, error)
}

// NewUSMT checks the tools are where the operator says they are.
func NewUSMT(dir string) (*USMT, error) {
	if dir == "" {
		return nil, fmt.Errorf("say where USMT is with --usmt: it is in the Windows ADK, under " +
			"Assessment and Deployment Kit\\User State Migration Tool\\amd64, and DSKY does not ship it")
	}
	for _, exe := range []string{"scanstate.exe", "loadstate.exe"} {
		if _, err := os.Stat(filepath.Join(dir, exe)); err != nil {
			return nil, fmt.Errorf("%s is not in %s — that folder should hold both scanstate.exe and "+
				"loadstate.exe, from the Windows ADK", exe, dir)
		}
	}
	return &USMT{Dir: dir, run: runCommand}, nil
}

// USMTTimeout is generous on purpose: a profile with twenty years of documents
// in it is a long copy, and a migration killed half way through is worse than
// one that took all afternoon.
const USMTTimeout = 8 * time.Hour

// Capture copies the named people's files off the old machine into the store.
func (u *USMT) Capture(ctx context.Context, m *Manifest, key string, out io.Writer) error {
	if m.Data.Strategy != DataUSMT {
		return fmt.Errorf("this plan does not move files with USMT (it says %q), so there is nothing to capture", m.Data.Strategy)
	}
	if m.Data.StorePath == "" {
		return fmt.Errorf("this plan names no store to copy into — set data.store_path and approve it again")
	}
	if len(m.Data.Users) == 0 {
		return fmt.Errorf("this plan names nobody to copy — set data.users and approve it again")
	}
	args, cleanup, err := u.args(m, key, true)
	if err != nil {
		return err
	}
	defer cleanup()
	fmt.Fprintf(out, "copying %d profile(s) from %s into %s\n", len(m.Data.Users), m.Source.Hostname, m.Data.StorePath)
	fmt.Fprintln(out, "  this reads the machine and writes to the store; nothing on this PC is changed")
	return u.exec(ctx, "scanstate.exe", args, out)
}

// Restore puts them back on the new machine.
func (u *USMT) Restore(ctx context.Context, m *Manifest, key string, out io.Writer) error {
	if m.Data.Strategy != DataUSMT {
		return fmt.Errorf("this plan does not move files with USMT (it says %q), so there is nothing to restore", m.Data.Strategy)
	}
	if m.Data.StorePath == "" {
		return fmt.Errorf("this plan names no store to restore from")
	}
	args, cleanup, err := u.args(m, key, false)
	if err != nil {
		return err
	}
	defer cleanup()
	fmt.Fprintf(out, "restoring %d profile(s) from %s\n", len(m.Data.Users), m.Data.StorePath)
	return u.exec(ctx, "loadstate.exe", args, out)
}

// args builds the command line for both halves, which differ less than they
// look: the same store, the same people, the same two migration rule files.
func (u *USMT) args(m *Manifest, key string, capturing bool) ([]string, func(), error) {
	args := []string{m.Data.StorePath}
	// MigApp.xml carries application settings, MigDocs.xml finds documents
	// wherever people actually put them rather than only in the known
	// folders. Both ship with USMT, beside the executables.
	for _, x := range []string{"MigApp.xml", "MigDocs.xml"} {
		args = append(args, "/i:"+filepath.Join(u.Dir, x))
	}
	// Exclude everybody, then name the people the plan names. The plan was
	// reviewed and approved by somebody who knew whose machine this was;
	// copying a profile nobody asked for is a privacy problem, not a bonus.
	if capturing {
		args = append(args, "/ue:*")
		for _, who := range m.Data.Users {
			args = append(args, "/ui:"+who)
		}
		args = append(args, "/o") // overwrite a store from an earlier attempt
	}
	// Carry on after the errors that are not worth stopping for -- a file
	// locked by something still running -- and log them.
	args = append(args, "/c", "/v:5")

	cleanup := func() {}
	if m.Data.Encrypted {
		if key == "" {
			return nil, cleanup, fmt.Errorf("this plan's store is encrypted, so it needs its key")
		}
		// A key file rather than /key: on the command line. Anybody on the
		// machine can read a command line out of the process list, and this
		// one unlocks every document the old PC had.
		f, err := os.CreateTemp("", "dsky-usmt-*.key")
		if err != nil {
			return nil, cleanup, err
		}
		cleanup = func() { os.Remove(f.Name()) }
		if err := os.Chmod(f.Name(), 0o600); err != nil {
			f.Close()
			cleanup()
			return nil, func() {}, err
		}
		if _, err := f.WriteString(key); err != nil {
			f.Close()
			cleanup()
			return nil, func() {}, err
		}
		f.Close()
		flag := "/decrypt"
		if capturing {
			flag = "/encrypt"
		}
		args = append(args, flag, "/keyfile:"+f.Name())
	}
	return args, cleanup, nil
}

// exec runs one of the two tools and reports what it said.
func (u *USMT) exec(ctx context.Context, exe string, args []string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, USMTTimeout)
	defer cancel()
	code, output, err := u.run(ctx, filepath.Join(u.Dir, exe), args...)
	if s := strings.TrimSpace(output); s != "" {
		fmt.Fprintln(out, s)
	}
	switch {
	case err != nil:
		return fmt.Errorf("%s did not run: %w", exe, err)
	case code != 0:
		// USMT's exit codes are documented and numerous; the number is what
		// somebody searches for, so it goes in the message rather than being
		// translated into a guess.
		return fmt.Errorf("%s exited %d — nothing has been moved; the log beside the store says why", exe, code)
	}
	return nil
}

func runCommand(ctx context.Context, name string, args ...string) (int, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	b, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(b), nil
	}
	if err != nil {
		return -1, string(b), err
	}
	return 0, string(b), nil
}
