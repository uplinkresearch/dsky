package recipe

import (
	"strings"
	"testing"
)

func verifyRecipe(mutate func(*WindowsSpec)) *Recipe {
	w := &WindowsSpec{
		EICfg:    &EICfg{Edition: "Professional"},
		Unattend: &UnattendSpec{Template: "t.tmpl", Vars: map[string]string{"admin_user": "user", "account_mode": "local"}},
		Firstboot: FirstbootSpec{Mode: "generate", Log: "firstboot.log",
			Steps: []Step{{Drivers: true}}},
	}
	if mutate != nil {
		mutate(w)
	}
	return &Recipe{
		Version: 1, ID: "test-verify",
		OS:      OSSpec{Type: OSWindows, Source: "win11", SourceMode: SourceISO},
		Target:  TargetSpec{Scheme: "mbr", Filesystem: "fat32", Size: "auto", Boot: "uefi-only"},
		Flash:   FlashSpec{Verify: "readback-sha256"},
		Windows: w,
	}
}

func renderVerify(r *Recipe, d ResolvedDrivers) string {
	return GenerateVerifyPS(r, d, func(ref string) (string, error) { return ref + ".msi", nil })
}

// TestVerifyChecksTheSilentFailures: the point of this script is the things
// nobody notices. A machine that installed cleanly and is in a workgroup, or
// has a yellow-banged NIC, looks exactly like a success.
func TestVerifyChecksTheSilentFailures(t *testing.T) {
	r := verifyRecipe(func(w *WindowsSpec) {
		w.Domain = &DomainSpec{Join: "corp.example.com", Username: "svc", Password: "${var:pw}"}
	})
	got := renderVerify(r, ResolvedDrivers{})

	for _, want := range []string{
		"ConfigManagerErrorCode -ne 0", // yellow bangs
		"PartOfDomain",                 // the workgroup case
		"Test-ComputerSecureChannel",   // joined but trust broken
		"corp.example.com",             // the right domain, not just any
		"DOMAIN-JOIN-FAILED",           // points at the evidence
		"firstboot.log",                // did the script even run
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the check never looks for %q", want)
		}
	}
	// It must be read-only: this runs on a machine somebody is about to hand
	// over, and a "verify" step that changes something is a trap.
	for _, forbidden := range []string{
		"Remove-Item", "Set-ItemProperty", "New-Item", "Stop-Process",
		"Restart-Computer", "Add-Computer", "Start-Process",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("the check calls %s — it must only report", forbidden)
		}
	}
}

// TestVerifyIsSilentAboutWhatWasNotAsked: a check that reports on a domain
// nobody asked to join, or programs nobody chose, is noise — and noise is how
// people learn to ignore the output.
func TestVerifyIsSilentAboutWhatWasNotAsked(t *testing.T) {
	got := renderVerify(verifyRecipe(nil), ResolvedDrivers{})
	for _, unwanted := range []string{"PartOfDomain", "Test-ComputerSecureChannel", "Uninstall\\*"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("a recipe with no domain and no apps still checks for %q", unwanted)
		}
	}
	// But the universal checks are always there.
	for _, want := range []string{"ConfigManagerErrorCode", "firstboot.log", "Get-LocalUser"} {
		if !strings.Contains(got, want) {
			t.Errorf("the always-applicable check %q is missing", want)
		}
	}
}

func TestVerifyChecksRequestedPrograms(t *testing.T) {
	r := verifyRecipe(func(w *WindowsSpec) {
		w.Apps = &AppsSpec{Winget: []string{"Google.Chrome", "7zip.7zip"}}
		w.Firstboot.Steps = []Step{{Drivers: true}, {Apps: true},
			{MSI: &RunItem{Ref: "app-macula"}}}
	})
	got := renderVerify(r, ResolvedDrivers{})
	for _, want := range []string{
		"Google.Chrome", "Chrome", // the id reported, the display name matched
		"7zip.7zip", "7zip", //
		"app-macula.msi",               // the operator's own installer, by staged name
		"CurrentVersion\\Uninstall\\*", // how it looks
	} {
		if !strings.Contains(got, want) {
			t.Errorf("programs check is missing %q", want)
		}
	}
}

// TestVerifyWithOwnInstallerAndNoWingetApps is the ordinary MSP recipe: an
// agent MSI at first boot and no public packages at all. The programs section
// is reached for the installer, so anything that assumes winget apps exist
// panics on a nil spec — which is how this was found.
func TestVerifyWithOwnInstallerAndNoWingetApps(t *testing.T) {
	r := verifyRecipe(func(w *WindowsSpec) {
		w.Apps = nil
		w.Firstboot.Steps = []Step{{Drivers: true}, {MSI: &RunItem{Ref: "app-macula"}}}
	})
	got := renderVerify(r, ResolvedDrivers{}) // must not panic
	if !strings.Contains(got, "app-macula.msi") {
		t.Error("the operator's own installer is not checked")
	}
	if strings.Contains(got, "is NOT installed") {
		t.Error("checked for winget packages when none were requested")
	}
}

// TestVerifyExitCodeIsUsable: this gets run across a bench of machines, so the
// answer has to be machine-readable, not just coloured text.
func TestVerifyExitCodeIsUsable(t *testing.T) {
	got := renderVerify(verifyRecipe(nil), ResolvedDrivers{})
	for _, code := range []string{"exit 0", "exit 1", "exit 2"} {
		if !strings.Contains(got, code) {
			t.Errorf("the check never returns %q, so it cannot be scripted over twenty machines", code)
		}
	}
	if !strings.Contains(got, "DOES NOT MATCH THE BUILD") {
		t.Error("a failing machine is not called out unmistakably")
	}
}

// TestVerifyTellsRunningApartFromFailed is the difference between a check
// people trust and one they learn to ignore. Installing programs at first boot
// takes minutes and needs the network, so someone running this at the first
// logon prompt would otherwise get a red report that only means "not yet".
func TestVerifyTellsRunningApartFromFailed(t *testing.T) {
	got := renderVerify(verifyRecipe(nil), ResolvedDrivers{})
	for _, want := range []string{
		"LastWriteTime",               // how recently first boot wrote
		"STILL RUNNING",               // said plainly
		"exit 2",                      // and distinguishable by a script
		"-Wait",                       // how to watch it finish
		"has not been written to for", // the genuinely-stuck wording
	} {
		if !strings.Contains(got, want) {
			t.Errorf("cannot tell a running first boot from a failed one: missing %q", want)
		}
	}
}

// TestStatusScreenOffByDefault: painting a machine's lock screen is intrusive
// and permanent-looking. Nothing happens unless it was asked for.
func TestStatusScreenOffByDefault(t *testing.T) {
	got := renderVerify(verifyRecipe(nil), ResolvedDrivers{})
	for _, unwanted := range []string{"PersonalizationCSP", "New-StatusImage", "Wallpaper"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("a recipe that did not ask for a status screen emitted %q", unwanted)
		}
	}
}

func TestStatusScreenPaintsTheResult(t *testing.T) {
	r := verifyRecipe(func(w *WindowsSpec) { w.StatusScreen = &StatusScreen{} })
	got := renderVerify(r, ResolvedDrivers{})

	for _, want := range []string{
		"New-StatusImage",    // drawn, so a failure list can be on it
		"IMAGING FAILED",     // legible from the doorway
		"READY",              //
		"PersonalizationCSP", // the lock screen, which works without GPO
		"Wallpaper",          // and the desktop, since first boot ends logged in
		"$script:failures",   // the actual failures, not a bare red cross
		"Set-StatusScreen",   //
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status screen is missing %q", want)
		}
	}
	// PowerShell variable names are case-insensitive, so a Graphics object in
	// $g collides with the [int]$G colour parameter and every draw call after
	// it fails on an Int32. Found by running it.
	if strings.Contains(got, "$g.DrawString") || strings.Contains(got, "$g = [System.Drawing.Graphics]") {
		t.Error("the Graphics object is in $g, which collides with the [int]$G colour parameter")
	}
	if !strings.Contains(got, "[int]$R, [int]$G, [int]$B") {
		t.Error("colour components are untyped - PowerShell cannot resolve the FromArgb overload")
	}
	// Losing the paint job must not lose the report or the exit code.
	if !strings.Contains(got, "could not set the status screen") {
		t.Error("a failure to paint the screen is not survivable")
	}
}

// TestStatusScreenRunsAfterFirstBootCompletes: the check treats a log with no
// completion line as "still running", so calling it before that line is
// written would paint "still running" forever and never show a real result.
func TestStatusScreenRunsAfterFirstBootCompletes(t *testing.T) {
	r := verifyRecipe(func(w *WindowsSpec) { w.StatusScreen = &StatusScreen{} })
	fb := renderFirstboot(t, r)
	done := strings.Index(fb, "first-boot script done")
	verify := strings.Index(fb, "verify.ps1")
	switch {
	case verify < 0:
		t.Fatal("first boot never runs the check, so nothing paints the screen")
	case done < 0:
		t.Fatal("first boot no longer logs completion")
	case verify < done:
		t.Error("the check runs before the completion line - it would always say 'still running'")
	}
}

func TestStatusScreenCanBeCleared(t *testing.T) {
	got := GenerateClearStatusScreen()
	for _, want := range []string{
		"PersonalizationCSP", "LockScreenImagePath", "Wallpaper", "status.jpg",
		// And it removes its own task, so a machine re-imaged later does not
		// inherit a stale one.
		"schtasks /Delete",
		// The Windows default goes back, rather than an empty value: a blank
		// wallpaper is a black desktop, which reads as a second fault to
		// whoever opens the box.
		`Web\Wallpaper\Windows\img0.jpg`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the clear script leaves %q behind", want)
		}
	}
}

// TestStatusScreenClearsItselfBeforeHandover is the property that makes this
// safe to ship. The technician reads the bench, shuts the machine down and
// sends it on; whoever opens the box must not find the imaging signal as their
// wallpaper, least of all a red one.
func TestStatusScreenClearsItselfBeforeHandover(t *testing.T) {
	got := renderVerify(verifyRecipe(func(w *WindowsSpec) {
		w.StatusScreen = &StatusScreen{}
	}), ResolvedDrivers{})

	// At startup, not at logon: a startup task runs before the logon screen
	// is drawn, so the end user never sees the lock screen at all.
	if !strings.Contains(got, "/SC ONSTART") {
		t.Error("the cleanup is not registered for startup - the end user would see the lock screen once")
	}
	// And RunOnce for the desktop, which lives in the user's hive where a
	// task running as SYSTEM cannot reach it.
	if !strings.Contains(got, "RunOnce") {
		t.Error("the desktop wallpaper is never cleared - SYSTEM cannot reach the user's hive")
	}
	if !strings.Contains(got, "clears itself on the next boot") {
		t.Error("the operator is not told it cleans up after itself")
	}

	// keep: true is the deliberate opt-out, for a machine that stays put.
	kept := renderVerify(verifyRecipe(func(w *WindowsSpec) {
		w.StatusScreen = &StatusScreen{Keep: true}
	}), ResolvedDrivers{})
	if strings.Contains(kept, "/SC ONSTART") || strings.Contains(kept, "RunOnce") {
		t.Error("status_screen.keep still registered a cleanup")
	}
	if !strings.Contains(kept, "status_screen.keep is set") {
		t.Error("with keep set, nothing says the screen will persist")
	}
}

// TestStatusScreenSuccessImageIsOptional: it must work for someone with no
// artwork at all, and a missing logo must degrade rather than lose the signal.
func TestStatusScreenSuccessImageIsOptional(t *testing.T) {
	plain := renderVerify(verifyRecipe(func(w *WindowsSpec) {
		w.StatusScreen = &StatusScreen{}
	}), ResolvedDrivers{})
	if !strings.Contains(plain, "'READY'") {
		t.Error("with no artwork there is no success screen at all")
	}

	branded := renderVerify(verifyRecipe(func(w *WindowsSpec) {
		w.StatusScreen = &StatusScreen{Success: "payload/logo.png"}
	}), ResolvedDrivers{})
	if !strings.Contains(branded, "logo.png") {
		t.Error("the operator's artwork is never looked for")
	}
	if !strings.Contains(branded, "'READY'") {
		t.Error("no fallback when the artwork is missing - a lost logo would cost the signal")
	}
}

func TestStatusScreenValidation(t *testing.T) {
	r := verifyRecipe(func(w *WindowsSpec) {
		w.StatusScreen = &StatusScreen{Success: "a.png", SuccessRef: "b"}
	})
	if err := r.Validate(); err == nil {
		t.Error("both success and success_ref were accepted")
	}
}

// TestVerifyUsesCRLF: it is a .ps1 written to FAT32 and run by Windows;
// everything else this tool generates for Windows is CRLF too.
func TestVerifyUsesCRLF(t *testing.T) {
	got := renderVerify(verifyRecipe(nil), ResolvedDrivers{})
	if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Error("the script has bare LF line endings")
	}
}

// TestVerifyEditionMatchesCaption: Win32_OperatingSystem says "Pro", the
// ei.cfg EditionID says "Professional". Comparing them directly would fail on
// a correctly built machine, which is the worst kind of false alarm.
func TestVerifyEditionMatchesCaption(t *testing.T) {
	for _, tc := range []struct{ edition, want string }{
		{"Professional", "Pro"},
		{"ProfessionalN", "Pro N"},
		{"Core", "Home"},
		{"Enterprise", "Enterprise"},
	} {
		if got := editionMatch(tc.edition); got != tc.want {
			t.Errorf("editionMatch(%q) = %q, want %q", tc.edition, got, tc.want)
		}
	}
}

// TestWingetDisplayHintIsSafeInARegex: the hint goes straight into a
// PowerShell -match, so an unescaped metacharacter would either throw or match
// the wrong thing.
func TestWingetDisplayHintIsSafeInARegex(t *testing.T) {
	cases := map[string]string{
		"Google.Chrome":                     "Chrome",
		"Microsoft.VisualStudioCode":        "VisualStudioCode",
		"Notepad++.Notepad++":               `Notepad\+\+`,
		"Adobe.Acrobat.Reader.64-bit":       "Acrobat Reader 64-bit",
		"TheDocumentFoundation.LibreOffice": "LibreOffice",
	}
	for pkg, want := range cases {
		if got := wingetDisplayHint(pkg); got != want {
			t.Errorf("wingetDisplayHint(%q) = %q, want %q", pkg, got, want)
		}
	}
}

// A record of what the media carries has to match what the media carries. One
// for a file nothing stages would have the machine reporting a program it
// never got, and trying to update it afterwards -- worse than recording
// nothing, because it reads as an answer.
func TestAnOfflineRecordMustNameSomethingOnTheMedia(t *testing.T) {
	base := func() *Recipe {
		return &Recipe{
			Version: 1, ID: "r", Name: "R",
			OS:     OSSpec{Type: OSWindows, Source: "windows-11", SourceMode: SourceISO},
			Flash:  FlashSpec{Verify: "readback-sha256"},
			Target: TargetSpec{Scheme: "mbr", Filesystem: "fat32", Boot: "uefi-only", Size: "auto"},
			Windows: &WindowsSpec{
				Unattend:  &UnattendSpec{Template: "t.tmpl"},
				Firstboot: FirstbootSpec{Mode: "generate"},
				Payload:   []PayloadItem{{Ref: "winget-google.chrome"}},
				Apps: &AppsSpec{Offline: []OfflineApp{
					{ID: "Google.Chrome", Version: "153.0", Ref: "winget-google.chrome"},
				}},
			},
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("a recipe whose record matches its payload was refused: %v", err)
	}
	r := base()
	r.Windows.Apps.Offline[0].Ref = "winget-somethingelse"
	if err := r.Validate(); err == nil {
		t.Error("a record naming a file the media does not carry was accepted")
	}
	r = base()
	r.Windows.Apps.Offline[0].ID = ""
	if err := r.Validate(); err == nil {
		t.Error("a record with no winget id was accepted, so nothing could ever update it")
	}
}
