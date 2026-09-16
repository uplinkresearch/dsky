//go:build windows

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
	"unsafe"
)

// The task description must ask for a standard-user token in the signed-in
// user's session: that is the whole reason this path exists, and an elevated
// task would fail exactly as the direct install did.
func TestTaskXMLAsksForAStandardUserToken(t *testing.T) {
	x := taskXML(`CORP\user`, `C:\Windows\Setup\Scripts\dsky-agent.exe`, `C:\job.json`, `C:\res.json`)
	for _, want := range []string{
		"<RunLevel>LeastPrivilege</RunLevel>",
		"<LogonType>InteractiveToken</LogonType>",
		"<UserId>CORP\\user</UserId>",
		// The arguments are XML-escaped; schtasks reads this as a document.
		`user-install &quot;C:\job.json&quot; &quot;C:\res.json&quot;`,
	} {
		if !strings.Contains(x, want) {
			t.Errorf("the task description is missing %q", want)
		}
	}
	// schtasks refuses a file it cannot read as Unicode.
	b := utf16LE(x)
	if len(b) < 2 || b[0] != 0xff || b[1] != 0xfe {
		t.Fatal("no byte-order mark")
	}
	dec := string(utf16.Decode(func() []uint16 {
		var u []uint16
		for i := 2; i+1 < len(b); i += 2 {
			u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
		}
		return u
	}()))
	if dec != x {
		t.Error("the encoded task description does not read back as it was written")
	}
}

// The unelevated half runs the job it was handed and reports its own result,
// so nothing has to be parsed out of Task Scheduler's localised output.
func TestUserJobReportsItsOwnResult(t *testing.T) {
	dir := t.TempDir()
	job := filepath.Join(dir, "job.json")
	res := filepath.Join(dir, "res.json")
	os.WriteFile(job, []byte(`{"exe":"cmd.exe","args":["/c","exit 7"]}`), 0o644)
	if err := RunUserJob(job, res); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(res)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"code":7`) {
		t.Errorf("the result was %s, want the job's own exit code", b)
	}
}

// A policy the machine allows is written; the debloat step reports the ones
// Windows refuses rather than spraying an error into the log.
func TestSetPolicyWritesTheRegistry(t *testing.T) {
	a := &Agent{}
	p := policy{Path: `HKCU\Software\DSKY-test`, Name: "Probe", DWord: 1}
	if err := a.setPolicy(p); err != nil {
		t.Fatalf("writing a value under HKCU: %v", err)
	}
	if err := a.setPolicy(policy{Path: `HKNOPE\Software\x`, Name: "n", DWord: 1}); err == nil {
		t.Error("an unknown hive was accepted")
	}
}

// The files the two copies pass between them must live where a standard user
// can write. C:\Windows\Setup\Scripts, where the agent itself lives, is
// read-only for one of them, which would fail the install for the one reason
// this path exists to avoid.
func TestUserHandoffFilesAreWritableByAStandardUser(t *testing.T) {
	job, res, task := userHandoffPaths()
	for _, p := range []string{job, res, task} {
		if strings.Contains(strings.ToLower(p), `\windows\setup\scripts`) {
			t.Errorf("%s is beside the agent, where a standard user cannot write", p)
		}
	}
	// The directory must actually take a file: this is the check that would
	// have caught the first version.
	if err := os.WriteFile(job, []byte("{}"), 0o644); err != nil {
		t.Errorf("writing the job: %v", err)
	}
	os.Remove(job)
}

// SendInput takes a size and refuses anything but the real INPUT's, silently.
// On 64-bit Windows INPUT is 40 bytes with the key fields at fixed offsets.
func TestTheKeystrokeStructureIsTheSizeWindowsExpects(t *testing.T) {
	var k keyInput
	if unsafe.Sizeof(k) != 40 {
		t.Errorf("keyInput is %d bytes; Windows' INPUT is 40, and SendInput would refuse it", unsafe.Sizeof(k))
	}
	if off := unsafe.Offsetof(k.vk); off != 8 {
		t.Errorf("the virtual key is at offset %d, want 8", off)
	}
	if off := unsafe.Offsetof(k.extraInfo); off != 24 {
		t.Errorf("dwExtraInfo is at offset %d, want 24", off)
	}
}
