package agent

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// StepSettings is the step name compose puts in a manifest's step list.
const StepSettings = "settings"

const stepSettings = StepSettings

// Settings is what a migration asks this machine to be set to, beyond the
// programs it installs. Each entry is already decided: the manifest's
// allowlist worked out what to run, a person approved it, and nothing here
// interprets a value again.
//
// The split into two lists is the whole design, and it is not a tidy-up.
// The agent runs at first boot as the local administrator, and half of what
// people notice about a machine lives in HKCU: file extensions, hidden files,
// the taskbar search box. Applying those here would set them for an account
// nobody uses and leave the person who actually sits down to a machine that
// looks nothing like their old one. So those wait, and run for whoever signs
// in -- which on a migrated machine is a domain user this agent will never see.
type Settings struct {
	// Machine is applied now, by the agent, and is the same for everybody.
	Machine []Action `json:"machine,omitempty"`
	// PerUser is applied at each person's first sign-in instead.
	PerUser []Action `json:"per_user,omitempty"`
}

// Action is one setting, already turned into the thing that sets it.
type Action struct {
	// Key is the manifest's stable name for the setting (power.plan), which
	// is what the log, the report and the verify diff all say.
	Key    string `json:"key"`
	Method string `json:"method"` // registry | powershell
	Ref    string `json:"ref"`    // HKCU\Path\Name=1, or a command line
}

// settingsStep applies what belongs to the machine and leaves the rest for
// the people who will use it.
func (a *Agent) settingsStep() {
	s := a.Manifest.Settings
	if s == nil || (len(s.Machine) == 0 && len(s.PerUser) == 0) {
		a.J.Info(stepSettings, "no settings to carry over")
		return
	}
	if len(s.Machine) > 0 {
		a.UI.Detail("applying " + itoa(len(s.Machine)) + " setting(s)")
	}
	done, failed := 0, 0
	for _, act := range s.Machine {
		if err := a.applyAction(act); err != nil {
			// One line per setting, naming the setting rather than the
			// registry path: whoever reads this is holding a plan that calls
			// it power.plan, not a path under CurrentVersion.
			a.J.Fail(stepSettings, "%s was not applied: %v", act.Key, err)
			failed++
			continue
		}
		done++
	}
	if len(s.Machine) > 0 {
		a.J.Info(stepSettings, "applied %d of %d setting(s) for this machine", done, done+failed)
	}
	if len(s.PerUser) > 0 || len(a.Manifest.sharedPrinters()) > 0 || len(a.Manifest.Drives) > 0 {
		a.stageUserSettings(s.PerUser, a.Manifest.sharedPrinters(), a.Manifest.Drives)
	}
}

// applyAction carries out one setting.
func (a *Agent) applyAction(act Action) error {
	switch act.Method {
	case "registry":
		p, err := parseRegistryRef(act.Ref)
		if err != nil {
			return err
		}
		return a.setPolicy(p)
	case "powershell":
		// The allowlist writes these itself -- powercfg and the Set-Win*
		// cmdlets -- so this is a fixed set of commands from this repository,
		// not something a scanned machine can put here.
		r := run(3*time.Minute, "powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", act.Ref)
		if !r.ok() {
			return fmt.Errorf("exited %d: %s", r.Code, trimOut(r.Out))
		}
		return nil
	default:
		return fmt.Errorf("no way to apply a %q setting", act.Method)
	}
}

// parseRegistryRef reads the allowlist's "HIVE\path\Name=value" form.
//
// A value that is not a number is written as a string, which is how a setting
// like a power scheme's GUID survives.
func parseRegistryRef(ref string) (policy, error) {
	at := strings.LastIndex(ref, "=")
	if at < 0 {
		return policy{}, fmt.Errorf("%q does not say what to set it to", ref)
	}
	path, value := ref[:at], ref[at+1:]
	slash := strings.LastIndex(path, `\`)
	if slash < 0 {
		return policy{}, fmt.Errorf("%q is not a registry path", ref)
	}
	p := policy{Path: path[:slash], Name: path[slash+1:]}
	if p.Name == "" {
		return policy{}, fmt.Errorf("%q names no value", ref)
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		p.String = value
		return p, nil
	}
	p.DWord = n
	return p, nil
}
