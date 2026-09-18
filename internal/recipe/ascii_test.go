package recipe

import (
	"fmt"
	"github.com/uplinkresearch/dsky/internal/testpwsh"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func fatRecipe() *Recipe {
	return &Recipe{
		Version: 1, ID: "fat", Name: "Everything on",
		OS:     OSSpec{Source: "win", Type: OSWindows, SourceMode: SourceISO},
		Target: TargetSpec{Scheme: "mbr", Filesystem: "fat32", VolumeLabel: "ESD-USB", Size: "auto", Boot: "uefi-only"},
		Windows: &WindowsSpec{
			EICfg:        &EICfg{Edition: "Professional", Channel: "Retail"},
			Unattend:     &UnattendSpec{Template: "t.tmpl"},
			Debloat:      &DebloatSpec{Preset: "aggressive"},
			Apps:         &AppsSpec{Winget: []string{"Google.Chrome", "7zip.7zip"}},
			StatusScreen: &StatusScreen{},
			Firstboot:    FirstbootSpec{Mode: "generate", Steps: []Step{{Drivers: true}, {Debloat: true}, {Apps: true}}},
		},
	}
}

func fatDrivers() ResolvedDrivers {
	var d ResolvedDrivers
	d.HasSweepable = true
	d.Cabs = append(d.Cabs, struct {
		File string
		Dir  string
	}{File: "pack.cab", Dir: "cabpack"})
	d.Extracts = append(d.Extracts,
		struct {
			File string
			Dir  string
			Args []string
		}{File: "sp142792.exe", Dir: "hp-old", Args: []string{"-pdf", "-e", "-s", `-f"{dir}"`}},
		struct {
			File string
			Dir  string
			Args []string
		}{File: "sp999999.exe", Dir: "hp-new", Args: []string{"/s", "/e", "/f", `"{dir}"`}})
	return d
}

func generatedScripts(t *testing.T) map[string]string {
	t.Helper()
	r := fatRecipe()
	drv := fatDrivers()
	fb, err := GenerateFirstboot(r, drv, func(ref string) (string, error) { return ref, nil })
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"firstboot.cmd":           fb,
		"apps.ps1":                GenerateAppsPS(r),
		"debloat.ps1":             GenerateDebloatPS(r),
		"verify.ps1":              GenerateVerifyPS(r, drv, func(ref string) (string, error) { return ref, nil }),
		"clear-status-screen.cmd": GenerateClearStatusScreen(),
		ModelInstallerScriptName:  ModelInstallerScriptFile(),
	}
}

// Windows PowerShell 5.1 reads BOM-less scripts in the ANSI code page, where
// a UTF-8 em dash ends in a curly quote. One inside a string in apps.ps1
// terminated it early and the whole script failed to parse — on the imaged
// machine only, never on a dev box. cmd.exe has its own OEM-codepage mangling
// and cannot take a BOM at all. So: everything first boot runs stays ASCII.
func TestGeneratedScriptsAreASCII(t *testing.T) {
	for name, content := range generatedScripts(t) {
		for i := 0; i < len(content); i++ {
			if content[i] > 127 {
				line := 1 + strings.Count(content[:i], "\n")
				t.Errorf("%s line %d: non-ASCII byte 0x%02x — Windows PowerShell 5.1 and cmd.exe read these scripts in a legacy code page", name, line, content[i])
				break
			}
		}
	}
}

// Every generated PowerShell script must parse. Checked with PowerShell's own
// parser when pwsh is installed (CI's Windows and the dev box have it).
func TestGeneratedPowerShellParses(t *testing.T) {
	pwsh := testpwsh.Find()
	if pwsh == "" {
		t.Skip("no PowerShell that runs here")
	}
	dir := t.TempDir()
	for name, content := range generatedScripts(t) {
		if !strings.HasSuffix(name, ".ps1") {
			continue
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		check := fmt.Sprintf(
			`$e=$null; [void][System.Management.Automation.Language.Parser]::ParseFile('%s',[ref]$null,[ref]$e); if($e){$e|ForEach-Object{$_.Message}; exit 1}`, p)
		out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", check).CombinedOutput()
		if err != nil {
			t.Errorf("%s does not parse: %s", name, out)
		}
	}
}

// A package whose installer refuses an elevated context (winget 0x8A150056,
// Spotify and Discord among them) is retried as the signed-in user through a
// scheduled task with a standard-user token. First boot itself stays elevated
// because drivers need it.
func TestAppsInstallsPerUserPackagesUnelevated(t *testing.T) {
	apps := generatedScripts(t)["apps.ps1"]
	for _, want := range []string{
		"function Install-AsUser",
		"-LogonType Interactive -RunLevel Limited",
		"if ($code -eq -1978335146) {",
		"$code = Install-AsUser $id $base",
		"installed $id as the signed-in user",
		"Unregister-ScheduledTask",
		// The install writes its own exit code and that is what is read;
		// the task's own LastTaskResult reported success for a Spotify
		// install that never happened.
		`'Set-Content -Path "__RES__" -Value $LASTEXITCODE'`,
		"it never reported a result",
	} {
		if !strings.Contains(apps, want) {
			t.Errorf("apps.ps1 is missing %q", want)
		}
	}
	if strings.Contains(apps, "LastTaskResult") {
		t.Error("apps.ps1 still believes Task Scheduler's own result")
	}
	// do/while across a newline parses in pwsh 7 but the machine runs
	// Windows PowerShell 5.1, whose parser is older.
	for _, ln := range strings.Split(apps, "\n") {
		if strings.TrimSpace(ln) == "do { Start-Sleep -Seconds 5 }" {
			t.Error("do/while split across lines")
		}
	}
}

// The firstboot driver steps must judge extraction by whether .inf files
// appeared and retry with the other HP switch generation — the EliteBook
// pack printed usage and exited 0 when handed the old switches — and must
// keep %errorlevel% out of ( ) blocks, where cmd expands it at parse time.
func TestFirstbootDriverExtractFallback(t *testing.T) {
	fb := generatedScripts(t)["firstboot.cmd"]
	for _, want := range []string{
		`"%SCRIPTS%\sp142792.exe" -pdf -e -s -f"%SCRIPTS%\Drivers\hp-old"`,
		`retrying with /s /e /f "%SCRIPTS%\Drivers\hp-old"`,
		`"%SCRIPTS%\sp999999.exe" /s /e /f "%SCRIPTS%\Drivers\hp-new"`,
		`retrying with -pdf -e -s -f"%SCRIPTS%\Drivers\hp-new"`,
		`FAILED hp-old: no driver files extracted from sp142792.exe`,
		`dir /s /b "%SCRIPTS%\Drivers\*.inf" >nul 2>&1`,
		`no driver files to install`,
	} {
		if !strings.Contains(fb, want) {
			t.Errorf("firstboot.cmd is missing %q", want)
		}
	}
	// cmd expands %errorlevel% in a multi-line ( ) block when the block is
	// parsed, so an echo inside one logs the errorlevel from BEFORE the
	// block: the EliteBook's failed extract was logged as "exited with 0".
	depth := 0
	for _, ln := range strings.Split(fb, "\n") {
		trimmed := strings.TrimSpace(ln)
		if depth > 0 && strings.Contains(ln, "%errorlevel%") {
			t.Errorf("%%errorlevel%% inside a parenthesized block (parse-time expansion): %q", trimmed)
		}
		if strings.HasSuffix(trimmed, "(") && !strings.HasPrefix(trimmed, "rem") && !strings.HasPrefix(trimmed, "echo") {
			depth++
		}
		if trimmed == ")" && depth > 0 {
			depth--
		}
	}
}
