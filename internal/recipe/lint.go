package recipe

import (
	"fmt"
	"strings"
)

// Finding is one lint result.
type Finding struct {
	Severity string // "error" | "warning"
	Message  string
}

func (f Finding) String() string { return f.Severity + ": " + f.Message }

// agentMarkers identify RMM/remote-access installers by name. Baking one
// into an image (instead of first boot) clones its identity across machines
// — a hard-won production rule.
var agentMarkers = []string{
	"screenconnect", "connectwise", "teamviewer", "anydesk", "datto",
	"ninjaone", "ninjarmm", "atera", "kaseya", "syncro", "splashtop",
	"level.io", "tacticalrmm",
}

// Lint applies the production-lesson rules that Validate (structural) does
// not cover. Errors block builds; warnings print.
func (r *Recipe) Lint() []Finding {
	var out []Finding
	errf := func(format string, args ...any) {
		out = append(out, Finding{"error", fmt.Sprintf(format, args...)})
	}
	warnf := func(format string, args ...any) {
		out = append(out, Finding{"warning", fmt.Sprintf(format, args...)})
	}

	if r.Windows == nil {
		return out
	}
	w := r.Windows

	// SetupComplete.cmd is skipped when Setup runs under a firmware OEM key;
	// FirstLogonCommands is the reliable hook. Nothing may stage it.
	for _, p := range w.Payload {
		name := strings.ToLower(p.Path)
		if p.Ref != "" {
			name = strings.ToLower(p.Ref)
		}
		if strings.HasSuffix(name, "setupcomplete.cmd") {
			errf("payload stages SetupComplete.cmd — Windows skips it under firmware OEM keys; use firstboot steps (FirstLogonCommands) instead")
		}
	}

	// Agents run at first boot, never baked into media outside a firstboot
	// step (identity collisions in the RMM when cloned).
	firstbootRefs := map[string]bool{}
	for _, s := range w.Firstboot.Steps {
		if s.MSI != nil {
			firstbootRefs[strings.ToLower(s.MSI.Ref)] = true
		}
		if s.Exe != nil {
			firstbootRefs[strings.ToLower(s.Exe.Ref)] = true
		}
	}
	checkAgent := func(ref, where string) {
		low := strings.ToLower(ref)
		for _, marker := range agentMarkers {
			if strings.Contains(low, marker) && !firstbootRefs[low] {
				warnf("%s %q looks like an RMM/agent installer but no firstboot msi/exe step runs it — agents must install at first boot, never be pre-installed in media", where, ref)
			}
		}
	}
	for _, p := range w.Payload {
		if p.Ref != "" {
			checkAgent(p.Ref, "payload ref")
		}
		if p.Path != "" {
			checkAgent(p.Path, "payload path")
		}
	}
	for _, d := range w.DriverPacks {
		checkAgent(d.Ref, "driver_packs ref")
	}

	// Plaintext secrets in git-tracked YAML.
	if w.Unattend != nil {
		for k, v := range w.Unattend.Vars {
			if strings.Contains(strings.ToLower(k), "password") && v != "" && !strings.Contains(v, "${var:") {
				warnf("unattend var %q holds a literal password — use \"${var:%s}\" and put the value in vars.local.yaml (gitignored)", k, k)
			}
		}
	}

	// Domain join. The failure everything here guards against is the quiet
	// one: Setup carries on into a workgroup, the machine looks perfectly
	// installed, and nobody notices until someone tries a domain login.
	if d := w.Domain; d.Enabled() {
		if !d.Offline() {
			// Cleartext is not a choice here. The PlainText/base64 obfuscation
			// that local-account passwords can use does not exist for this
			// element — Microsoft documents that only local account passwords
			// can be hidden in an answer file. Setup scrubs the copy it caches
			// on the installed system; nothing scrubs the media.
			warnf("windows.domain uses a join account, so its password is written into " +
				"autounattend.xml on the stick in clear text — there is no way to obfuscate it, and " +
				"nothing removes it from the media, so the stick carries a live domain credential for " +
				"as long as it exists. Delegate an account that may only create computer objects in " +
				"the target OU, never a domain admin. An offline join (windows.domain.blob) avoids " +
				"putting a user credential on the stick at all")
			if d.Password != "" && !strings.Contains(d.Password, "${var:") {
				warnf("windows.domain.password holds a literal password — use \"${var:domain_password}\" " +
					"and put the value in vars.local.yaml (gitignored)")
			}
			if d.OU == "" {
				warnf("windows.domain sets no ou — the computer object lands in the domain's default " +
					"Computers container, where most group policy does not apply")
			}
		} else {
			// Not a user credential, but not nothing either: the blob carries
			// the machine account's password, and Microsoft says to treat it
			// as securely as a plaintext password. Worth saying, because
			// "offline join is the safe one" is easy to over-read.
			warnf("windows.domain uses an offline-join blob, which needs no domain privileges at " +
				"install time — but the blob still holds that computer account's password and " +
				"belongs to exactly one machine, so treat the stick as carrying a credential and " +
				"build one stick per machine")
		}
	}

	// A generate-mode firstboot that never installs drivers but stages
	// driver packs is almost certainly a mistake.
	if w.Firstboot.Mode == "generate" && len(w.DriverPacks) > 0 {
		hasDrivers := false
		for _, s := range w.Firstboot.Steps {
			if s.Drivers {
				hasDrivers = true
			}
		}
		if !hasDrivers {
			warnf("driver_packs are staged but firstboot.steps has no `drivers` step — they would never install")
		}
	}
	return out
}
