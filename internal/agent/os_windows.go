//go:build windows

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/uplinkresearch/dsky/internal/elevate"
)

// shell32 is loaded for the one call the agent makes into the shell: telling
// it a folder changed.
var shell32 = windows.NewLazySystemDLL("shell32.dll")

// ensureResume registers the task that starts the agent again at the next
// sign-in. It is called at the start of every run, so an interruption at any
// point is covered, and it is safe to call repeatedly.
//
// The trigger is a sign-in rather than a boot because the work needs a user:
// winget arrives as a per-user package, and the installers that refuse an
// administrator are run in the signed-in user's session. It runs as the user
// who is signed in now -- the account the answer file created -- with the
// highest token they have, which is what the first-boot run already had.
//
// Registering during the first sign-in does not start a second copy: a
// sign-in trigger fires on sign-ins after the task exists, and this session's
// happened before it.
func (a *Agent) ensureResume() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	user := os.Getenv("USERDOMAIN") + `\` + os.Getenv("USERNAME")
	xmlPath := filepath.Join(os.TempDir(), "dsky-resume-task.xml")
	if err := os.WriteFile(xmlPath, utf16LE(resumeTaskXML(user, self, a.Dir, a.Opts.Args())), 0o644); err != nil {
		return err
	}
	defer os.Remove(xmlPath)
	if r := run(2*time.Minute, "schtasks", "/create", "/tn", resumeTask, "/xml", xmlPath, "/f"); !r.ok() {
		return fmt.Errorf("schtasks: %s", trimOut(r.Out))
	}
	return nil
}

// clearResume takes the task away once there is nothing left to carry on
// with. A machine that has finished should not be running anything of ours at
// every sign-in for the rest of its life.
func (a *Agent) clearResume() {
	if r := run(2*time.Minute, "schtasks", "/delete", "/tn", resumeTask, "/f"); !r.ok() {
		a.J.Info("", "could not remove the %s task: %s", resumeTask, trimOut(r.Out))
	}
}

// armCatchUp registers the task that retries the program updates at every
// sign-in until they are all current.
//
// It runs as the person using the machine, with their highest token, for the
// same reason the resume task does: winget arrives as a per-user package and
// there is no winget for SYSTEM to run.
func (a *Agent) armCatchUp() {
	script := filepath.Join(dskyStateDir(), catchUpScript)
	if _, err := os.Stat(script); err != nil {
		a.J.Info(stepCatchUp, "no update script to arm: %v", err)
		return
	}
	user := os.Getenv("USERDOMAIN") + `\` + os.Getenv("USERNAME")
	xmlPath := filepath.Join(os.TempDir(), "dsky-catchup-task.xml")
	if err := os.WriteFile(xmlPath, utf16LE(catchUpTaskXML(user, script)), 0o644); err != nil {
		a.J.FailDetail(stepCatchUp, "could not write the update task", err.Error())
		return
	}
	defer os.Remove(xmlPath)
	if r := run(2*time.Minute, "schtasks", "/create", "/tn", catchUpTask, "/xml", xmlPath, "/f"); !r.ok() {
		a.J.FailDetail(stepCatchUp, "could not register the update task", trimOut(r.Out))
		return
	}
	a.J.Info(stepCatchUp, "this PC has no internet yet, so the programs from the media are as old as the media. "+
		"They will be updated at the next sign-in after it is connected, or by running %s", script)
}

// clearCatchUp takes the task away once every program is current.
func (a *Agent) clearCatchUp() {
	run(2*time.Minute, "schtasks", "/delete", "/tn", catchUpTask, "/f")
}

// disarmAutoLogon takes away the automatic sign-in the answer file set up for
// provisioning, and the password it stored to do it.
//
// The answer file asks for enough automatic sign-ins to cover the first boot
// and the restarts the agent may need. Windows spends them by counting down,
// so a machine that finishes in one boot would keep the rest -- signing itself
// in, with the password still in the registry, on the next machine's owner's
// first two boots. Provisioning arms this; provisioning puts it away.
func (a *Agent) disarmAutoLogon() {
	const path = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon`
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.SET_VALUE)
	if err != nil {
		a.J.Info("", "could not open Winlogon to turn off the automatic sign-in: %v", err)
		return
	}
	defer k.Close()
	cleared := 0
	for _, name := range []string{"AutoAdminLogon", "AutoLogonCount", "DefaultPassword"} {
		switch err := k.DeleteValue(name); {
		case err == nil:
			cleared++
		case errors.Is(err, registry.ErrNotExist), errors.Is(err, syscall.ERROR_FILE_NOT_FOUND):
			// Never set, or Windows has already spent it.
		default:
			a.J.Info("", "could not clear %s: %v", name, err)
		}
	}
	// AutoAdminLogon is the switch; without it Windows asks for a password
	// whatever else is left behind.
	if err := k.SetStringValue("AutoAdminLogon", "0"); err != nil {
		a.J.Info("", "could not turn the automatic sign-in off: %v", err)
		return
	}
	a.J.Info("", "automatic sign-in turned off (%d value(s) cleared); this machine asks for a password from now on", cleared)
}

// restart asks Windows to go down, with enough warning that somebody standing
// over the machine can stop it.
func (a *Agent) restart(reason string) error {
	r := run(2*time.Minute, "shutdown", "/r", "/t", itoa(restartDelaySeconds), "/c", "DSKY: "+reason)
	if !r.ok() {
		return fmt.Errorf("shutdown: %s", trimOut(r.Out))
	}
	return nil
}

// machineModel reads what the machine says about itself, for driver packs
// that are only for one model. The BIOS keys are what the model gate script
// used and are there before any vendor software is installed.
func machineModel() (vendor, model string) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\BIOS`, registry.QUERY_VALUE)
	if err != nil {
		return "", ""
	}
	defer k.Close()
	vendor, _, _ = k.GetStringValue("SystemManufacturer")
	model, _, _ = k.GetStringValue("SystemProductName")
	return vendor, model
}

// userJob is one install handed to an unelevated copy of the agent.
type userJob struct {
	Exe  string   `json:"exe"`
	Args []string `json:"args"`
}

// userResult is what that copy reports back.
type userResult struct {
	Code int    `json:"code"`
	Out  string `json:"out"`
	Err  string `json:"err,omitempty"`
}

// runAsSignedInUser runs a program as the signed-in user with a standard-user
// token, for installers that refuse to run elevated.
//
// It works by starting the agent again through Task Scheduler, rather than by
// asking Task Scheduler to run winget and then reading the task's status:
// task status and last-result text are localised, and a machine imaged in
// another language would have had its results misread. The second copy writes
// its own result to a file, so nothing is parsed out of console output.
func (a *Agent) runAsSignedInUser(exe string, args []string) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	jobPath, resPath, xmlPath := userHandoffPaths()
	os.Remove(resPath)
	b, err := json.Marshal(userJob{Exe: exe, Args: args})
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(jobPath, b, 0o644); err != nil {
		return 0, err
	}
	defer os.Remove(jobPath)

	user := os.Getenv("USERDOMAIN") + `\` + os.Getenv("USERNAME")
	if err := os.WriteFile(xmlPath, utf16LE(taskXML(user, self, jobPath, resPath)), 0o644); err != nil {
		return 0, err
	}
	defer os.Remove(xmlPath)

	const taskName = "DSKY-user-install"
	if r := run(2*time.Minute, "schtasks", "/create", "/tn", taskName, "/xml", xmlPath, "/f"); !r.ok() {
		return 0, fmt.Errorf("could not register the task: %s", trimOut(r.Out))
	}
	defer run(2*time.Minute, "schtasks", "/delete", "/tn", taskName, "/f")

	if r := run(2*time.Minute, "schtasks", "/run", "/tn", taskName); !r.ok() {
		return 0, fmt.Errorf("could not start the task: %s", trimOut(r.Out))
	}

	// Long enough for a big installer on a slow line -- the longest install
	// watched in the VM took four and a half minutes -- and short enough
	// that a machine somebody is standing over does not sit silent while a
	// task that will never report is waited out.
	deadline := time.Now().Add(userInstallDeadline)
	for {
		if b, err := os.ReadFile(resPath); err == nil {
			var res userResult
			if err := json.Unmarshal(b, &res); err != nil {
				return 0, fmt.Errorf("the standard-user install wrote an unreadable result: %w", err)
			}
			os.Remove(resPath)
			a.J.Raw(res.Out)
			if res.Err != "" {
				return res.Code, fmt.Errorf("%s", res.Err)
			}
			return res.Code, nil
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("the standard-user install did not report a result within %s", userInstallDeadline)
		}
		time.Sleep(5 * time.Second)
	}
}

// userHandoffPaths are the files the two copies of the agent pass between
// them. Not beside the agent: C:\Windows\Setup\Scripts is read-only for a
// standard user, and the whole point of this path is that the second copy
// has a standard-user token. The user's own temp directory is the same
// directory for both, because elevation does not change it.
func userHandoffPaths() (job, result, task string) {
	d := os.TempDir()
	return filepath.Join(d, "dsky-user-job.json"),
		filepath.Join(d, "dsky-user-result.json"),
		filepath.Join(d, "dsky-user-task.xml")
}

// RunUserJob is the unelevated half: the agent started again by Task
// Scheduler in the signed-in user's session. It runs the one command it was
// given and writes the result where the elevated copy is waiting.
func RunUserJob(jobPath, resultPath string) error {
	b, err := os.ReadFile(jobPath)
	if err != nil {
		return err
	}
	var job userJob
	if err := json.Unmarshal(b, &job); err != nil {
		return err
	}
	r := run(25*time.Minute, job.Exe, job.Args...)
	res := userResult{Code: r.Code, Out: r.Out}
	if r.Err != nil {
		res.Err = r.Err.Error()
	}
	out, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return os.WriteFile(resultPath, out, 0o644)
}

// taskXML describes a one-off task that runs as the given user with a
// standard-user token. RunLevel LeastPrivilege is the whole point.
func taskXML(user, exe, jobPath, resultPath string) string {
	esc := func(s string) string {
		r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
		return r.Replace(s)
	}
	args := fmt.Sprintf(`user-install "%s" "%s"`, jobPath, resultPath)
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>DSKY: install a program that refuses to run elevated</Description>
  </RegistrationInfo>
  <Principals>
    <Principal id="Author">
      <UserId>` + esc(user) + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>false</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <ExecutionTimeLimit>PT25M</ExecutionTimeLimit>
    <Priority>7</Priority>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + esc(exe) + `</Command>
      <Arguments>` + esc(args) + `</Arguments>
    </Exec>
  </Actions>
</Task>
`
}

// utf16LE encodes with a byte-order mark: schtasks /xml rejects a file it
// cannot recognise as Unicode.
func utf16LE(s string) []byte {
	out := []byte{0xff, 0xfe}
	for _, r := range utf16.Encode([]rune(s)) {
		out = append(out, byte(r), byte(r>>8))
	}
	return out
}

// setPolicy writes one registry value.
func (a *Agent) setPolicy(p policy) error {
	root, path, ok := splitHive(p.Path)
	if !ok {
		return fmt.Errorf("%s: unknown registry hive", p.Path)
	}
	k, _, err := registry.CreateKey(root, path, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if p.String != "" {
		return k.SetStringValue(p.Name, p.String)
	}
	return k.SetDWordValue(p.Name, uint32(p.DWord))
}

func splitHive(path string) (registry.Key, string, bool) {
	switch {
	case strings.HasPrefix(path, `HKLM\`):
		return registry.LOCAL_MACHINE, path[5:], true
	case strings.HasPrefix(path, `HKCU\`):
		return registry.CURRENT_USER, path[5:], true
	}
	return 0, "", false
}

// removeAppx takes the consumer apps off the machine. Appx packages have no
// command-line interface worth the name, so this is the one place the agent
// still uses PowerShell — with a fixed script written by the agent, not text
// assembled per build, and handed its list as a file so nothing has to be
// quoted into a command line.
func (a *Agent) removeAppx(prefixes []string) {
	listPath := filepath.Join(a.Dir, "dsky-appx-list.txt")
	if err := os.WriteFile(listPath, []byte(strings.Join(prefixes, "\r\n")+"\r\n"), 0o644); err != nil {
		a.J.FailDetail(stepDebloat, "could not write the app list", err.Error())
		return
	}
	defer os.Remove(listPath)
	scriptPath := filepath.Join(a.Dir, "dsky-appx.ps1")
	// The byte-order mark is deliberate: Windows PowerShell reads a file
	// without one in the machine's legacy code page.
	if err := os.WriteFile(scriptPath, append([]byte("\xef\xbb\xbf"), appxScript...), 0o644); err != nil {
		a.J.FailDetail(stepDebloat, "could not write the removal script", err.Error())
		return
	}
	defer os.Remove(scriptPath)

	r := run(30*time.Minute, "powershell", "-NoProfile", "-NonInteractive",
		"-ExecutionPolicy", "Bypass", "-File", scriptPath, "-ListFile", listPath)
	a.J.Raw(r.Out)
	removed := 0
	for _, ln := range strings.Split(r.Out, "\n") {
		ln = strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(ln, "REMOVED "):
			removed++
		case strings.HasPrefix(ln, "FAILED "):
			a.J.Fail(stepDebloat, "%s", strings.TrimPrefix(ln, "FAILED "))
		}
	}
	if r.Err != nil || (!r.ok() && removed == 0) {
		a.J.FailDetail(stepDebloat, "removing consumer apps did not run", trimOut(r.Out))
		return
	}
	a.J.Info(stepDebloat, "removed %d consumer app(s)", removed)
}

// appxScript is constant, so it is the same text on every build and can be
// checked once. It prints one line per package, which the agent counts.
const appxScript = `param([Parameter(Mandatory=$true)][string]$ListFile)
$ErrorActionPreference = 'Continue'
$names = Get-Content -LiteralPath $ListFile | Where-Object { $_.Trim() -ne '' }
$prov = @(Get-AppxProvisionedPackage -Online)
$installed = @(Get-AppxPackage -AllUsers)
foreach ($name in $names) {
  foreach ($p in ($prov | Where-Object { $_.DisplayName -like "$name*" })) {
    try {
      Remove-AppxProvisionedPackage -Online -PackageName $p.PackageName -ErrorAction Stop | Out-Null
      Write-Output ("REMOVED provisioned " + $p.DisplayName)
    } catch {
      Write-Output ("FAILED deprovision " + $p.DisplayName + ": " + $_.Exception.Message)
    }
  }
  foreach ($p in ($installed | Where-Object { $_.Name -like "$name*" })) {
    try {
      Remove-AppxPackage -AllUsers -Package $p.PackageFullName -ErrorAction Stop
      Write-Output ("REMOVED " + $p.Name)
    } catch {
      Write-Output ("FAILED remove " + $p.Name + ": " + $_.Exception.Message)
    }
  }
}
Write-Output "APPX DONE"
`

// desktopDirs is every desktop on the machine: the one all users see, each
// account's own, any that OneDrive has taken over, and the Default profile --
// the one that matters most, because a shortcut left there is copied onto the
// desktop of every account made afterwards.
func desktopDirs() []string {
	var dirs []string
	add := func(p string) {
		if p == "" {
			return
		}
		for _, have := range dirs {
			if strings.EqualFold(have, p) {
				return
			}
		}
		dirs = append(dirs, p)
	}
	if p := os.Getenv("PUBLIC"); p != "" {
		add(filepath.Join(p, "Desktop"))
	}
	if p := os.Getenv("USERPROFILE"); p != "" {
		add(filepath.Join(p, "Desktop"))
		add(filepath.Join(p, "OneDrive", "Desktop"))
		// Every other profile on the machine, Default included.
		users := filepath.Dir(p)
		if entries, err := os.ReadDir(users); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					add(filepath.Join(users, e.Name(), "Desktop"))
				}
			}
		}
	}
	return dirs
}

// keepAwake stops Windows switching the display off or sleeping while the
// machine is being provisioned, and returns the function that lets it again.
//
// Watched in the VM: the status window was up and correct, and the screen was
// black, because Windows' idle timer had turned the display off during the
// program installs. Somebody walking up to that machine sees exactly what the
// window was built to spare them -- a machine that looks dead -- and a laptop
// that sleeps part way through a driver sweep is worse than one that looks
// dead.
//
// The request belongs to the thread that makes it and ends when that thread
// does. Go moves goroutines between threads, so it is made from a thread
// locked for the purpose and held until release.
func keepAwake() (release func()) {
	const (
		esContinuous      = 0x80000000
		esSystemRequired  = 0x00000001
		esDisplayRequired = 0x00000002
	)
	set := kernel32.NewProc("SetThreadExecutionState")
	done := make(chan struct{})
	held := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		set.Call(esContinuous | esSystemRequired | esDisplayRequired)
		close(held)
		<-done
		set.Call(esContinuous)
	}()
	<-held
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// verifySignature asks Windows whether a file's Authenticode signature holds,
// and who signed it. A fixed script handed the path as an argument, so nothing
// is quoted into a command line.
func verifySignature(file string) (status, subject string, err error) {
	script := filepath.Join(os.TempDir(), "dsky-signature.ps1")
	body := "param([Parameter(Mandatory=$true)][string]$Path)\r\n" +
		"$s = Get-AuthenticodeSignature -LiteralPath $Path\r\n" +
		"Write-Output ('STATUS ' + $s.Status)\r\n" +
		"if ($s.SignerCertificate) { Write-Output ('SIGNER ' + $s.SignerCertificate.Subject) }\r\n"
	if err := os.WriteFile(script, append([]byte("\xef\xbb\xbf"), body...), 0o644); err != nil {
		return "", "", err
	}
	defer os.Remove(script)
	r := run(5*time.Minute, "powershell", "-NoProfile", "-NonInteractive",
		"-ExecutionPolicy", "Bypass", "-File", script, "-Path", file)
	if r.Err != nil {
		return "", "", r.Err
	}
	for _, ln := range strings.Split(r.Out, "\n") {
		ln = strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(ln, "STATUS "):
			status = strings.TrimPrefix(ln, "STATUS ")
		case strings.HasPrefix(ln, "SIGNER "):
			subject = strings.TrimPrefix(ln, "SIGNER ")
		}
	}
	if status == "" {
		return "", "", fmt.Errorf("no answer from Get-AuthenticodeSignature: %s", trimOut(r.Out))
	}
	return status, subject, nil
}

// isElevated reports whether the agent holds an administrator's token.
func isElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// payloadRoot is where standalone payloads are kept while they run.
func payloadRoot() string {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, "DSKY", "payloads")
}

// refreshDesktop tells the shell that these folders changed, so the icons a
// sweep removed stop being drawn.
//
// Watched in the VM: a payload removed VLC's desktop shortcut, the file was
// gone, and the icon stayed on the desktop until something else made Explorer
// look again. An icon that opens "item not found" is worse than the shortcut
// would have been.
func refreshDesktop(dirs []string) {
	const (
		shcneUpdateDir = 0x00001000
		shcnfPathW     = 0x0005
		shcnfFlush     = 0x1000
	)
	notify := shell32.NewProc("SHChangeNotify")
	for _, dir := range dirs {
		p, err := windows.UTF16PtrFromString(dir)
		if err != nil {
			continue
		}
		notify.Call(shcneUpdateDir, shcnfPathW|shcnfFlush, uintptr(unsafe.Pointer(p)), 0)
	}
}

// runElevatedExe starts a program with an administrator's token, one UAC
// prompt, and returns what it exited with. A payload that arrived as one file
// is double-clicked by a person, so asking Windows is the only way it gets the
// rights it needs; telling them to find PowerShell is not an answer.
func runElevatedExe(exe string, args []string) (int, error) {
	return elevate.RunElevatedExe(exe, args)
}

// scratchRoot is where the agent is unpacked to be elevated: the user's own
// temporary folder, since this part runs as them and needs no rights at all.
func scratchRoot() string {
	return filepath.Join(os.TempDir(), "DSKY")
}
