package agent

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// payloadFile writes a file that is both a program and a zip: some bytes
// standing in for the agent, then the archive. This is the shape compose
// builds, so the reader is tested against the thing it will meet.
func payloadFile(t *testing.T, prefix []byte, files map[string][]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "office-apps-payload.exe")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(prefix); err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	zw.SetOffset(int64(len(prefix)))
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// openPayload reads the payload out of a file and closes it when the test
// ends. Left open, Windows refuses to delete the file and the test fails in
// its own cleanup -- which is how the leak that locked a payload's stick for
// the length of a run was found.
func openPayload(t *testing.T, path string) *payload {
	t.Helper()
	p, err := attachedPayload(path)
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Cleanup(func() { p.Close() })
	}
	return p
}

// standaloneJSON is a payload's manifest as it is carried inside the file.
func standaloneJSON(t *testing.T) []byte {
	t.Helper()
	m := standaloneManifest()
	m.Build = "abc123"
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The payload is read out of the running program's own file: the bytes ahead
// of the archive are the program, and a zip reader finds the archive from its
// own end regardless of what is in front of it.
func TestAPayloadIsReadOutOfTheProgramItself(t *testing.T) {
	prefix := bytes.Repeat([]byte("MZ this is the agent"), 5000)
	path := payloadFile(t, prefix, map[string][]byte{
		ManifestName:  standaloneJSON(t),
		AgentName:     []byte("agent bytes"),
		"Drivers/x.i": []byte("inf"),
	})
	zr := openPayload(t, path)
	if zr == nil {
		t.Fatal("no payload was found in the file")
	}
	m, err := manifestFrom(zr.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Standalone() || m.Build != "abc123" {
		t.Errorf("manifest read as %+v", m)
	}

	dir := t.TempDir()
	var lastDone, total int64
	if err := extract(zr.Reader, dir, func(done, tot int64) { lastDone, total = done, tot }); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "Drivers", "x.i")); err != nil || string(got) != "inf" {
		t.Errorf("the driver file came out as %q: %v", got, err)
	}
	if lastDone != total || total == 0 {
		t.Errorf("progress ended at %d of %d", lastDone, total)
	}
}

// An ordinary agent staged beside a manifest on a stick carries no payload,
// and saying it does would send a first boot down the wrong path entirely.
func TestAnOrdinaryAgentCarriesNoPayload(t *testing.T) {
	plain := filepath.Join(t.TempDir(), "dsky-agent.exe")
	if err := os.WriteFile(plain, bytes.Repeat([]byte("MZ not an archive"), 100), 0o755); err != nil {
		t.Fatal(err)
	}
	if zr := openPayload(t, plain); zr != nil {
		t.Error("a plain agent was read as carrying a payload")
	}
	// A zip with no manifest is somebody else's archive, not ours.
	other := payloadFile(t, []byte("MZ"), map[string][]byte{"notes.txt": []byte("hello")})
	if zr := openPayload(t, other); zr != nil {
		t.Error("an archive with no manifest was read as a payload")
	}
}

// An entry that would write outside the folder is refused. We build these
// files, which is exactly why: the day one is edited by hand or built by
// something else, the answer must be a refusal rather than a file written
// over Windows.
func TestAPayloadCannotWriteOutsideItsFolder(t *testing.T) {
	for _, name := range []string{"../escaped.exe", "a/../../escaped.exe", `..\escaped.exe`, "/etc/passwd"} {
		path := payloadFile(t, []byte("MZ"), map[string][]byte{
			ManifestName: standaloneJSON(t),
			name:         []byte("x"),
		})
		zr := openPayload(t, path)
		if zr == nil {
			t.Fatalf("%s: no payload", name)
		}
		dir := t.TempDir()
		err := extract(zr.Reader, dir, nil)
		if err == nil || !strings.Contains(err.Error(), "outside the folder") {
			t.Errorf("%s was extracted rather than refused: %v", name, err)
		}
	}
}

// Running the same payload again does not write a gigabyte again: a file
// already there at the same size is left alone.
func TestExtractingTwiceDoesNotRewriteWhatIsThere(t *testing.T) {
	path := payloadFile(t, []byte("MZ"), map[string][]byte{
		ManifestName: standaloneJSON(t),
		AgentName:    []byte("agent bytes"),
	})
	zr := openPayload(t, path)
	dir := t.TempDir()
	if err := extract(zr.Reader, dir, nil); err != nil {
		t.Fatal(err)
	}
	agentPath := filepath.Join(dir, AgentName)
	st, err := os.Stat(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	before := st.ModTime()
	if err := extract(zr.Reader, dir, nil); err != nil {
		t.Fatal(err)
	}
	st, err = os.Stat(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(before) {
		t.Error("a file already there at the same size was written again")
	}
}

// Double-clicked with no administrator's token, it asks Windows for one and
// hands back what that run exited with -- rather than writing a thing to the
// machine and failing at the first driver.
func TestAPayloadAsksForAnAdministratorBeforeItWritesAnything(t *testing.T) {
	path := payloadFile(t, []byte("MZ"), map[string][]byte{
		ManifestName: standaloneJSON(t),
		AgentName:    []byte("agent bytes"),
	})
	zr := openPayload(t, path)
	home := t.TempDir()
	swap(t, &payloadRootFn, func() string { return home })
	swap(t, &isElevatedFn, func() bool { return false })
	var asked []string
	swap(t, &elevateFn, func(args []string) (int, error) { asked = args; return 3, nil })
	ran := false
	swap(t, &runPayloadCopyFn, func(exe string, args []string) (int, error) { ran = true; return 0, nil })

	problems, err := startAttached(zr, RunOptions{Quiet: true, Unattended: true})
	if err != nil {
		t.Fatal(err)
	}
	if problems != 3 {
		t.Errorf("the elevated run reported 3 problems, this one reported %d", problems)
	}
	if ran {
		t.Error("the payload ran without an administrator's token")
	}
	if strings.Join(asked, " ") != "apply --quiet --unattended" {
		t.Errorf("the elevated run was started as %v, losing the options it was given", asked)
	}
	if entries, err := os.ReadDir(home); err == nil && len(entries) > 0 {
		t.Errorf("it wrote %d thing(s) to the machine before asking", len(entries))
	}
}

// With a token it unpacks onto the machine and hands over to the copy that is
// now there -- which is what a restart comes back to, since the file it
// arrived in may have been on a stick.
func TestAPayloadUnpacksOntoTheMachineAndRunsFromThere(t *testing.T) {
	path := payloadFile(t, []byte("MZ"), map[string][]byte{
		ManifestName:      standaloneJSON(t),
		AgentName:         []byte("agent bytes"),
		"Drivers/x.inf":   []byte("[Version]"),
		"installer.msi":   []byte("msi"),
		"Run DSKY.cmd":    []byte("@echo off"),
		"README.txt":      []byte("read me"),
		"nested/deep.bin": []byte("deep"),
	})
	zr := openPayload(t, path)
	root := t.TempDir()
	swap(t, &payloadRootFn, func() string { return root })
	swap(t, &isElevatedFn, func() bool { return true })
	swap(t, &elevateFn, func([]string) (int, error) {
		t.Error("it asked for an administrator when it already had one")
		return 0, nil
	})
	var gotExe string
	var gotArgs []string
	swap(t, &runPayloadCopyFn, func(exe string, args []string) (int, error) {
		gotExe, gotArgs = exe, args
		return 2, nil
	})

	problems, err := startAttached(zr, RunOptions{Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if problems != 2 {
		t.Errorf("problems came back as %d, not what the run reported", problems)
	}
	home := filepath.Join(root, "office-apps-abc123")
	if gotExe != filepath.Join(home, AgentName) {
		t.Errorf("it handed over to %s, not the copy on the machine", gotExe)
	}
	if strings.Join(gotArgs, " ") != "apply "+home+" --quiet" {
		t.Errorf("the copy was started as %v", gotArgs)
	}
	for _, name := range []string{AgentName, ManifestName, "Drivers/x.inf", "installer.msi", "nested/deep.bin"} {
		if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(name))); err != nil {
			t.Errorf("%s did not reach the machine: %v", name, err)
		}
	}
}

// A first-boot manifest in one of these files is refused: its steps assume a
// machine with no owner, and this one has somebody using it.
func TestAFirstBootManifestIsNotRunAsAPayload(t *testing.T) {
	b, err := json.Marshal(&Manifest{Version: ManifestVersion, Recipe: "office-apps"})
	if err != nil {
		t.Fatal(err)
	}
	path := payloadFile(t, []byte("MZ"), map[string][]byte{ManifestName: b, AgentName: []byte("x")})
	zr := openPayload(t, path)
	swap(t, &isElevatedFn, func() bool { return true })
	swap(t, &runPayloadCopyFn, func(string, []string) (int, error) {
		t.Error("a first-boot payload was run on a machine in use")
		return 0, nil
	})
	if _, err := startAttached(zr, RunOptions{Quiet: true}); err == nil ||
		!strings.Contains(err.Error(), "install media") {
		t.Errorf("refused with %v", err)
	}
}

// Double-clicked, with no arguments at all, it runs the payload it carries.
// Every other program named on a command line with no arguments prints its
// usage, and printing usage at somebody who double-clicked a payload is the
// same as doing nothing.
func TestNoArgumentsRunsTheCarriedPayload(t *testing.T) {
	path := payloadFile(t, []byte("MZ"), map[string][]byte{
		ManifestName: standaloneJSON(t),
		AgentName:    []byte("agent bytes"),
	})
	// A fresh read per run, as a real process gets: the file is opened, its
	// contents come out, and the handle is let go before the work starts.
	swap(t, &selfPayloadFn, func() (*payload, error) { return openPayload(t, path), nil })
	swap(t, &payloadRootFn, func() string { return t.TempDir() })
	swap(t, &isElevatedFn, func() bool { return true })
	ran := false
	swap(t, &runPayloadCopyFn, func(string, []string) (int, error) { ran = true; return 0, nil })
	swap(t, &openScreenFn, func(string, bool) *screen { return nil })

	if err := Main(nil); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("double-clicking a payload did nothing")
	}

	// Options with no command are a script running the same file, where
	// apply is the only thing they could have meant.
	ran = false
	if err := Main([]string{"--quiet", "--unattended"}); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Error("a payload run with options and no command did nothing")
	}

	// And an agent that carries nothing still says what it is for.
	swap(t, &selfPayloadFn, func() (*payload, error) { return nil, nil })
	if err := Main(nil); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Errorf("a plain agent with no arguments said %v", err)
	}
	if err := Main([]string{"--quiet"}); err == nil || !strings.Contains(err.Error(), "carries no payload") {
		t.Errorf("a plain agent given bare options said %v", err)
	}
}
