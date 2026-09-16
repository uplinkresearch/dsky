package agent

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// A payload can be one file: the agent with the payload's zip appended to it.
// Windows runs the program and ignores the bytes after it; a zip reader reads
// the zip and ignores the bytes before it, because a zip says where its
// contents are relative to its own end. So the same file is both, and there is
// nothing to extract by hand before it will run.
//
// That matters more than it sounds. The folder form asks somebody to extract a
// zip and find the right file inside it, and Explorer will happily run the
// launcher from inside the un-extracted zip, in a temporary folder, beside
// none of the files it needs. One file that can only be double-clicked has no
// wrong way to start it.
//
// It is also still a zip: anything that opens zips opens it, and out comes the
// folder form, which is what a remote tool that cannot run a program with a
// window wants.

// payload is the archive inside this file, and the handle it is read through.
// The handle has to be closed as soon as the files are out: Windows will not
// let an open file be deleted, moved, or its drive ejected, and a payload run
// from a stick would hold that stick for the length of the run.
type payload struct {
	*zip.Reader
	f *os.File
}

func (p *payload) Close() error {
	if p == nil || p.f == nil {
		return nil
	}
	return p.f.Close()
}

// attachedPayload returns the zip appended to this executable, or nil when
// there is none -- an ordinary agent staged beside a manifest on a stick.
func attachedPayload(exe string) (*payload, error) {
	f, err := os.Open(exe)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		// Not a zip at all: the usual case, and not an error.
		f.Close()
		return nil, nil
	}
	if _, err := zr.Open(ManifestName); err != nil {
		// A zip with no manifest is not one of ours. Say so rather than
		// unpacking somebody else's archive over a machine.
		f.Close()
		return nil, nil
	}
	return &payload{Reader: zr, f: f}, nil
}

// manifestFrom reads the manifest out of an attached payload, which is how the
// run knows where the payload belongs before a single file is written.
func manifestFrom(zr *zip.Reader) (*Manifest, error) {
	f, err := zr.Open(ManifestName)
	if err != nil {
		return nil, fmt.Errorf("this payload has no %s: %w", ManifestName, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return parseManifest(b)
}

// extract writes the payload out to dir. It reports progress by bytes, since
// the files differ by three orders of magnitude -- a manifest and a gigabyte
// of drivers -- and counting files would sit at "2 of 9" for ten minutes.
//
// A file already there at the same size is left alone, so double-clicking the
// same payload twice does not write a gigabyte twice.
func extract(zr *zip.Reader, dir string, progress func(done, total int64)) error {
	var total, done int64
	for _, f := range zr.File {
		total += int64(f.UncompressedSize64)
	}
	if progress != nil {
		progress(0, total)
	}
	for _, f := range zr.File {
		name, err := safeJoin(dir, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(name, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return err
		}
		size := int64(f.UncompressedSize64)
		if st, err := os.Stat(name); err == nil && st.Size() == size {
			done += size
			if progress != nil {
				progress(done, total)
			}
			continue
		}
		if err := extractFile(f, name); err != nil {
			return err
		}
		done += size
		if progress != nil {
			progress(done, total)
		}
	}
	return nil
}

// extractFile writes one entry, whole or not at all: a half-written agent that
// looks finished because it is the right name is worse than no agent.
func extractFile(f *zip.File, name string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp := name + ".partial"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	// Copy, then close, then rename: the CRC is checked as the last bytes are
	// read, so a truncated or corrupted payload fails here rather than part
	// way through installing from it.
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("%s did not come out of this payload whole: %w", f.Name, err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, name)
}

// safeJoin refuses a zip entry that would write outside dir. We build these
// zips ourselves, which is exactly why this is here: the day something else
// builds one, or a payload is edited by hand, the answer has to be a refusal
// rather than a file written over C:\Windows.
func safeJoin(dir, name string) (string, error) {
	if strings.Contains(name, `\`) || path.IsAbs(name) || strings.HasPrefix(name, "../") ||
		strings.Contains(name, "/../") || name == ".." || strings.HasSuffix(name, "/..") {
		return "", fmt.Errorf("this payload holds an entry that would write outside the folder: %q", name)
	}
	out := filepath.Join(dir, filepath.FromSlash(name))
	if rel, err := filepath.Rel(dir, out); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("this payload holds an entry that would write outside the folder: %q", name)
	}
	return out, nil
}

// elevateFn asks Windows for an administrator's token by starting this
// program again, as a variable so the tests never raise a UAC prompt.
var elevateFn = runElevated

// startAttached runs a payload that arrived as one file: elevate, unpack onto
// the machine, and hand over to the copy that is now on it. The copy is what
// runs the payload, and what a resume task starts after a restart, because
// this file may be on a stick that is gone by then.
func startAttached(p *payload, opts RunOptions) (problems int, err error) {
	m, err := manifestFrom(p.Reader)
	if err != nil {
		return 0, err
	}
	if !m.Standalone() {
		return 0, errors.New("this file carries a first-boot payload, which belongs on install media rather than on a machine already in use")
	}
	// Elevated first, before anything is written: the payload's home is under
	// ProgramData, and every step after it -- drivers, installers, removing
	// consumer apps -- needs an administrator anyway. Asking at the start is
	// one prompt at the moment somebody is watching, rather than a failure
	// half an hour in.
	if !isElevatedFn() {
		code, err := elevateFn(append([]string{"apply"}, opts.Args()...))
		if err != nil {
			return 0, err
		}
		return code, nil
	}

	home := payloadHome(m)
	if err := os.MkdirAll(home, 0o755); err != nil {
		return 0, fmt.Errorf("could not make a place for this payload on the machine: %w", err)
	}
	ui := openUnpackScreen(opts)
	err = extract(p.Reader, home, func(done, total int64) {
		ui.progress(done, total)
	})
	ui.close()
	// Let go of the file the moment its contents are out. What runs from here
	// is the copy on the machine, and the run takes half an hour: holding the
	// original open that long locks whatever it came on.
	if cerr := p.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	return runPayloadCopyFn(filepath.Join(home, AgentName), append([]string{"apply", home}, opts.Args()...))
}

// unpackScreen is the window while the payload is being written to the disk.
// Unpacking a gigabyte takes long enough that a machine with nothing on the
// screen looks like a machine where nothing happened -- which is what somebody
// reported the first time the status window was slow to appear.
type unpackScreen struct {
	s    *screen
	last int
}

func openUnpackScreen(opts RunOptions) *unpackScreen {
	if opts.Quiet {
		return &unpackScreen{}
	}
	s := openScreenFn(machineName(machineModel()), false)
	if s == nil {
		return &unpackScreen{}
	}
	s.Doing("Unpacking", "putting this payload on the machine")
	return &unpackScreen{s: s, last: -1}
}

func (u *unpackScreen) progress(done, total int64) {
	if u == nil || u.s == nil || total <= 0 {
		return
	}
	pct := int(done * 100 / total)
	if pct == u.last {
		return
	}
	u.last = pct
	u.s.Detail(fmt.Sprintf("%d%% of %s", pct, humanBytes(total)))
}

func (u *unpackScreen) close() {
	if u == nil || u.s == nil {
		return
	}
	u.s.Finished("Unpacking", 0)
	u.s.Close()
}

// humanBytes is for a person reading a progress line, so one decimal and no
// argument about whether a gigabyte is a billion bytes.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", n/1024)
	}
}

// selfPayloadFn is how the run looks inside its own file, as a variable so
// the tests can put a payload there without building an executable.
var selfPayloadFn = func() (*payload, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return attachedPayload(exe)
}

func selfPayload() (*payload, error) { return selfPayloadFn() }
