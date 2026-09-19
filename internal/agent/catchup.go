package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Catching up the programs that came off the stick.
//
// A machine built offline gets its programs from the media: the installers
// were downloaded on the day the stick was made and have been sitting on it
// ever since. That is the whole point -- a PC with no network still comes out
// of setup with Chrome on it -- and it is also the catch. A stick built in
// March installs March's Chrome in July.
//
// It matters less than it looks, because these are the real vendor installers
// and every one of them writes the Add/Remove Programs entry the vendor
// writes, so winget recognises the package afterwards and can upgrade it like
// any other. Chrome and Firefox update themselves regardless. The rest need
// somebody to say so, once.
//
// So the agent does this:
//
//   - It writes down what came off the stick and what version, in a file a
//     technician can read, because "what was this machine built with?" is
//     otherwise unanswerable on a machine nobody watched being built. That
//     file records the media, and never changes afterwards; what the catch-up
//     did goes in its own log; whether anything is still pending is answered
//     by whether the task still exists. One fact, one place, each of them.
//   - If the machine has the internet at first boot after all -- plugged in at
//     the bench, or the wifi worked -- it upgrades them there and then, and
//     the staleness never existed.
//   - If it does not, it leaves a script somebody can run by hand and a task
//     that runs it at every sign-in until there is nothing left to update.
//     The task takes itself away at that point: a machine that has caught up
//     should not be running anything of ours for the rest of its life.

const stepCatchUp = "catch-up"

// catchUpTask is the registered task's name, and the name to look for on a
// machine that is behaving oddly.
const catchUpTask = "DSKY-update-programs"

// catchUpScript is what the task runs, and what a technician runs by hand.
// Named for what it does rather than for DSKY, because the person who finds
// it is usually not the person who built the stick.
const catchUpScript = "update-programs.cmd"

// offlineRecordName is the record of what came off the media.
const offlineRecordName = "offline-programs.json"

// OfflineRecord is what a machine built offline carries about its programs.
type OfflineRecord struct {
	// BuiltAt is when the media was made, which is the age of every version
	// below. Left empty by older media.
	BuiltAt  string         `json:"built_at,omitempty"`
	Programs []OfflineEntry `json:"programs"`
}

// OfflineEntry is one program that came off the stick rather than the vendor.
type OfflineEntry struct {
	ID      string `json:"id"`      // the winget package id
	Version string `json:"version"` // the version the media carried
	File    string `json:"file"`    // what it was called on the stick
}

// catchUpStep is run after the installers. It records what came off the media
// and brings it up to date, now if the machine can and later if it cannot.
func (a *Agent) catchUpStep() {
	apps := a.Manifest.Apps
	if apps == nil || len(apps.Offline) == 0 {
		return
	}
	rec := OfflineRecord{BuiltAt: apps.BuiltAt}
	for _, o := range apps.Offline {
		rec.Programs = append(rec.Programs, OfflineEntry{ID: o.ID, Version: o.Version, File: o.File})
	}
	if err := a.writeCatchUpFiles(rec); err != nil {
		a.J.FailDetail(stepCatchUp, "could not write the record of what came off the media", err.Error())
		return
	}
	a.J.Info(stepCatchUp, "%d program(s) came off the media; the versions are recorded in %s",
		len(rec.Programs), filepath.Join(dskyStateDir(), offlineRecordName))

	// The network is asked about first, and quickly. findWinget waits five
	// minutes for App Installer to finish registering, which is right when
	// there are packages to install and pointless on a machine that has no
	// internet to install them from.
	if !a.online() {
		a.armCatchUp()
		return
	}
	wg := a.findWinget()
	if wg == "" {
		a.armCatchUp()
		return
	}
	a.UI.Detail("checking the programs from the stick for updates")
	if !a.runCatchUp(wg, rec) {
		a.armCatchUp()
		return
	}
	a.clearCatchUp()
	a.J.Info(stepCatchUp, "every program from the media is current, so nothing is left to run later")
}

// wingetNoUpgrade is winget reporting that a package is already at the newest
// version it knows about. Distinct from "nothing applicable to do", which is
// what it says when the package is not installed in a form it can act on.
const wingetNoUpgrade = -1978335212 // 0x8A150014

// dskyStateDir is where DSKY leaves things the machine keeps: the record of
// what came off the media, the catch-up script, its log. ProgramData rather
// than a profile, because none of it belongs to one person.
func dskyStateDir() string {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "DSKY")
}

// online asks once whether the machine can reach anything, rather than
// waiting the five minutes waitOnline does. The catch-up is the optional half
// of the offline install: a machine built offline probably still has no
// network, and standing there for five minutes to confirm it helps nobody.
func (a *Agent) online() bool {
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get("http://www.msftconnecttest.com/connecttest.txt")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// runCatchUp upgrades each recorded package, and reports whether all of them
// ended up current. A package winget cannot see yet -- the source list has not
// refreshed, the machine lost the network half way -- is not a failure, it is
// a reason to try again later.
func (a *Agent) runCatchUp(wg string, rec OfflineRecord) bool {
	all := true
	for _, p := range rec.Programs {
		r := run(20*time.Minute, wg, "upgrade", "--id", p.ID, "--exact", "--silent",
			"--accept-package-agreements", "--accept-source-agreements", "--disable-interactivity")
		switch {
		case r.Err != nil:
			a.J.FailDetail(stepCatchUp, p.ID+": the update did not finish", r.Err.Error())
			all = false
		case r.Code == 0:
			a.J.Info(stepCatchUp, "%s updated from the version on the media (%s)", p.ID, p.Version)
		case r.Code == wingetAlreadyInstalled || r.Code == wingetNoUpgrade:
			a.J.Info(stepCatchUp, "%s is current at %s", p.ID, p.Version)
		default:
			a.J.FailDetail(stepCatchUp, p.ID+": could not be updated (exit "+itoa(r.Code)+")", trimOut(r.Out))
			all = false
		}
	}
	return all
}

// writeCatchUpFiles puts the record and the script where a technician can find
// them: one folder, under ProgramData, not in anybody's profile.
func (a *Agent) writeCatchUpFiles(rec OfflineRecord) error {
	dir := dskyStateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, offlineRecordName), append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, catchUpScript), []byte(catchUpScriptText(rec)), 0o755)
}

// catchUpScriptText is the script the task runs and a technician can run by
// hand. It is a .cmd on purpose: no execution policy to argue with, nothing of
// ours has to survive on the machine for it to work, and whoever finds it can
// read every line of it before running it.
//
// It takes its own sign-in task away once every package reports nothing left
// to do, so a machine that has caught up stops running this at every sign-in.
// The script itself stays: it is a few hundred bytes, it costs nothing, and
// the next technician who wants to re-check this machine has it to hand.
func catchUpScriptText(rec OfflineRecord) string {
	ids := make([]string, 0, len(rec.Programs))
	for _, p := range rec.Programs {
		ids = append(ids, p.ID)
	}
	sort.Strings(ids)

	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\r\n", args...) }
	w(`@echo off`)
	w(`rem Bring this PC's programs up to date.`)
	w(`rem`)
	w(`rem They were installed from the setup media because this machine had no`)
	w(`rem internet when it was set up, so they are as old as the media. This`)
	w(`rem asks winget to update each of them. It is safe to run at any time,`)
	w(`rem and it deletes itself once there is nothing left to update.`)
	w(`setlocal`)
	w(`set "LOG=%%ProgramData%%\DSKY\update-programs.log"`)
	w(`set "PENDING=0"`)
	w(`echo [%%DATE%% %%TIME%%] update-programs started>>"%%LOG%%"`)
	w(``)
	w(`rem winget arrives as a per-user package, so it is not always on PATH.`)
	w(`set "WINGET=winget"`)
	w(`where winget >nul 2>&1 || set "WINGET=%%LOCALAPPDATA%%\Microsoft\WindowsApps\winget.exe"`)
	w(`if not exist "%%WINGET%%" if "%%WINGET%%" neq "winget" (`)
	w(`  echo [%%DATE%% %%TIME%%] winget is not available on this PC yet>>"%%LOG%%"`)
	w(`  exit /b 1`)
	w(`)`)
	w(``)
	for _, id := range ids {
		w(`call :update %s`, id)
	}
	w(``)
	w(`if "%%PENDING%%"=="0" (`)
	w(`  echo [%%DATE%% %%TIME%%] everything is current, nothing left to do>>"%%LOG%%"`)
	w(`  schtasks /delete /tn "%s" /f >nul 2>&1`, catchUpTask)
	w(`)`)
	w(`exit /b 0`)
	w(``)
	w(`:update`)
	w(`echo [%%DATE%% %%TIME%%] %%1>>"%%LOG%%"`)
	w(`"%%WINGET%%" upgrade --id %%1 --exact --silent --accept-package-agreements --accept-source-agreements --disable-interactivity >>"%%LOG%%" 2>&1`)
	w(`rem 0 updated; -1978335189 nothing applicable; -1978335212 no upgrade available.`)
	w(`if "%%ERRORLEVEL%%"=="0" goto :eof`)
	w(`if "%%ERRORLEVEL%%"=="%d" goto :eof`, wingetAlreadyInstalled)
	w(`if "%%ERRORLEVEL%%"=="%d" goto :eof`, wingetNoUpgrade)
	w(`echo [%%DATE%% %%TIME%%] %%1 exited %%ERRORLEVEL%%, will try again later>>"%%LOG%%"`)
	w(`set "PENDING=1"`)
	w(`goto :eof`)
	return b.String()
}

// catchUpTaskXML describes the task that retries the updates: at every
// sign-in, and once a day for a machine that is left signed in for a month.
//
// StartWhenAvailable and a network condition are deliberately not used to
// decide whether to run. The script itself is what knows whether anything is
// still out of date, and it is the thing that removes the task; a task that
// Windows quietly declines to start on a network it thinks is metered would
// leave a machine stale with nothing saying why.
func catchUpTaskXML(user, script string) string {
	esc := func(s string) string {
		r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
		return r.Replace(s)
	}
	// A start date in the past: Windows requires one, and any machine this
	// runs on has a clock set by the time first boot is over.
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>DSKY: update the programs that were installed from the setup media, once this PC has the internet</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>` + esc(user) + `</UserId>
      <Delay>PT2M</Delay>
    </LogonTrigger>
    <CalendarTrigger>
      <Enabled>true</Enabled>
      <StartBoundary>2020-01-01T12:00:00</StartBoundary>
      <ScheduleByDay>
        <DaysInterval>1</DaysInterval>
      </ScheduleByDay>
    </CalendarTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + esc(user) + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>HighestAvailable</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>true</Hidden>
    <ExecutionTimeLimit>PT2H</ExecutionTimeLimit>
    <Priority>7</Priority>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + esc(script) + `</Command>
    </Exec>
  </Actions>
</Task>
`
}
