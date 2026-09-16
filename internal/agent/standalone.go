package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// A standalone payload is the agent deployed onto a machine that already runs
// Windows -- the programs, and whatever else a recipe asks for, without
// installing an operating system. It is the same agent as a first boot uses,
// handed a manifest that says so.
//
// It arrives on a stick or a network share, which may be read-only and will
// not be there after a restart. So before anything else it copies itself to
// the machine and runs from the copy: the resume task has something to start
// after a restart, and the log and the record of what is done live somewhere
// that stays.

// The parts that touch the real machine, as variables for the tests.
var (
	isElevatedFn     = isElevated
	payloadRootFn    = payloadRoot
	runPayloadCopyFn = runPayloadCopy
	exitFn           = os.Exit
)

// payloadHome is where one payload lives while it runs. Named for the recipe
// and the build, so running the same payload again carries on and a newer one
// starts fresh.
func payloadHome(m *Manifest) string {
	return filepath.Join(payloadRootFn(), safeName(m.Recipe+"-"+m.Build))
}

// startStandalone gets a payload ready to run: elevated, and on the machine.
// ranElsewhere means a copy of the agent did the run, and problems is what it
// reported.
func startStandalone(dir string, m *Manifest, opts RunOptions) (ranElsewhere bool, problems int, err error) {
	if !isElevatedFn() {
		return false, 0, errors.New("this payload changes the whole machine, so it has to run as an administrator: " +
			"use \"Run DSKY.cmd\" beside it, or start PowerShell with Run as administrator")
	}
	home := payloadHome(m)
	if samePath(dir, home) {
		return false, 0, nil
	}
	if err := copyPayload(dir, home); err != nil {
		return true, 0, fmt.Errorf("could not copy the payload onto this machine: %w", err)
	}
	code, err := runPayloadCopyFn(filepath.Join(home, "dsky-agent.exe"), append([]string{"apply", home}, opts.Args()...))
	return true, code, err
}

// copyPayload copies a payload onto the machine, leaving out the records of
// earlier runs, which belong to wherever those runs happened. A file already
// there at the same size is left alone, so a payload started again from its
// stick after a restart does not copy a gigabyte of drivers twice.
func copyPayload(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		switch d.Name() {
		case LogName, EventsName, StateName:
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if st, err := os.Stat(target); err == nil && st.Size() == info.Size() {
			return nil
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".partial"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// runPayloadCopy starts the agent's own copy and waits for it, passing its
// output and its exit code straight through, so whoever started the payload
// sees one run.
func runPayloadCopy(exe string, args []string) (int, error) {
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 0, err
}

// safeName keeps a recipe id and build usable as one folder name.
func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// parseApplyArgs reads `apply [dir] [--quiet] [--unattended]`, in any order.
func parseApplyArgs(args []string) (dir string, opts RunOptions, err error) {
	for _, a := range args {
		switch a {
		case "--quiet":
			opts.Quiet = true
		case "--unattended":
			opts.Unattended = true
		default:
			if strings.HasPrefix(a, "--") {
				return "", opts, fmt.Errorf("unknown option %s (known: --quiet, --unattended)", a)
			}
			if dir != "" {
				return "", opts, fmt.Errorf("apply takes one directory, got %q and %q", dir, a)
			}
			dir = a
		}
	}
	return dir, opts, nil
}

// exitWith ends the process with the problem count, so a remote tool can tell
// a clean run from one that needs looking at. Capped, because exit codes are
// small and 100 problems is already the same answer as 300.
func exitWith(problems int) {
	if problems > 0 {
		exitFn(min(problems, 100))
	}
}
