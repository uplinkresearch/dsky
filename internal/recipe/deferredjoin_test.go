package recipe

import (
	"strings"
	"testing"
)

// A recipe whose first boot DSKY generates joins the domain from that script,
// not from Windows Setup — and restarts itself into the rest of the build.
//
// Joining during Setup is what leaves a PC at a sign-in screen with nothing
// installed: Windows will not sign a local account in automatically on a
// machine that has just joined a domain, so the first-boot script never runs
// at all.
func TestAGeneratedFirstBootJoinsTheDomainItself(t *testing.T) {
	r := &Recipe{Version: 1, ID: "front-desk", Name: "Front desk",
		OS: OSSpec{Type: "windows", Source: "win11", SourceMode: SourceISO},
		Windows: &WindowsSpec{
			Domain:    &DomainSpec{Blob: "NEWDESK01.txt"},
			Firstboot: FirstbootSpec{Mode: "generate", Log: "firstboot.log", Steps: []Step{{Drivers: true}}},
		}}
	out, err := GenerateFirstboot(r, ResolvedDrivers{}, func(ref string) (string, error) { return ref, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"djoin /requestodj", // the join itself
		"/localos",          // against the running system, not an image
		DomainBlobFile,      // from the file staged beside the script
		DomainJoinMarker,    // one attempt only
		"RunOnce",           // the resume
		"shutdown /r",       // the restart the join needs
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the generated first boot has no %q:\n%s", want, out)
		}
	}
	// The blob is that computer's domain password and does not outlive its use.
	if !strings.Contains(out, `del /f /q "%SCRIPTS%\`+DomainBlobFile) {
		t.Errorf("the join file is left on the disk:\n%s", out)
	}
	// The join must come before the check that reports a machine in a
	// workgroup, or the first pass would always report failure.
	if strings.Index(out, "djoin /requestodj") > strings.Index(out, "DOMAIN JOIN FAILED") {
		t.Error("the join happens after the check that it happened")
	}
	// And the restart is an exit, not something the rest of the script runs past.
	join := out[strings.Index(out, "djoin /requestodj"):]
	if i, j := strings.Index(join, "shutdown /r"), strings.Index(join, "exit /b 0"); i < 0 || j < i {
		t.Errorf("the script carries on after asking for a restart:\n%s", join[:400])
	}

	// With no domain there is none of it, so every recipe that worked before
	// generates what it did.
	plain := &Recipe{Version: 1, ID: "p", Name: "P",
		OS:      OSSpec{Type: "windows", Source: "win11", SourceMode: SourceISO},
		Windows: &WindowsSpec{Firstboot: FirstbootSpec{Mode: "generate", Log: "firstboot.log", Steps: []Step{{Drivers: true}}}}}
	out, err = GenerateFirstboot(plain, ResolvedDrivers{}, func(ref string) (string, error) { return ref, nil })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "djoin") || strings.Contains(out, "RunOnce") {
		t.Errorf("a build with no domain grew a join:\n%s", out)
	}
}
