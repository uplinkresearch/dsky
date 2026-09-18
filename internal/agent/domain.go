package agent

import (
	"os"
	"path/filepath"
	"time"
)

// StepDomain is the step name compose puts in a manifest's step list.
const StepDomain = "domain"

const stepDomain = StepDomain

// Domain is an offline domain join the agent performs, rather than Setup.
//
// The obvious place for an offline join is the answer file: Setup applies the
// blob in the specialize pass and the machine is a domain member before
// anything else runs. That is what DSKY did, and on a machine that also has to
// sign itself in to finish provisioning, it does not work -- not "sometimes",
// not "slowly": the machine never reaches a desktop at all.
//
// What the lab showed, from the machine's own OOBE log: the answer file is
// honoured in full, the local account is created and named as the automatic
// sign-in, and then msoobe overrides it -- "Not setting autologon for new
// local user. e.g. upgrade, domain-joined, or system-managed user" -- and
// hands the sign-in to defaultuser0 to run user OOBE instead. User OOBE has
// nothing it can complete without somebody in front of the screen, so the OOBE
// monitor times out, resets the image state from COMPLETE back to
// SPECIALIZE_RESEAL_TO_OOBE, and reboots into OOBE. Forever. The "Why did my
// PC restart?" page a machine sits on is that reset explaining itself.
//
// So the join waits until the machine is up and somebody -- this agent -- is
// signed in. That is not a workaround invented here: a by-serial batch has
// always joined this way at first boot, which is exactly why that path never
// had the problem. The blob is the same blob, made by the same
// `djoin /provision`, and still carries no user credential.
type Domain struct {
	// File is the join file beside the agent, made by `djoin /provision` for
	// this one machine.
	//
	// There is deliberately no domain name here. A recipe's offline join
	// carries only the blob: naming a domain alongside it is what makes a
	// join credentialed (recipe.DomainSpec), and a credentialed join is a
	// different thing with a password in it. The name is inside the blob, and
	// the machine says which domain it joined once it has; until then "the
	// domain" is all this can honestly print.
	File string `json:"file"`
}

// domainStep applies the offline join and asks for the restart that makes the
// machine a domain member.
func (a *Agent) domainStep() {
	d := a.Manifest.Domain
	if d == nil || d.File == "" {
		a.J.Info(stepDomain, "no domain join in this build")
		return
	}
	blob := filepath.Join(a.Dir, d.File)
	if _, err := os.Stat(blob); err != nil {
		a.J.Fail(stepDomain, "%s is not beside the agent, so this machine cannot join the domain: %v",
			d.File, err)
		return
	}

	// /localos is what makes this the running system rather than an offline
	// image, and it is the difference between joining and a confusing
	// "the specified path is not a Windows directory".
	win := os.Getenv("SystemRoot")
	if win == "" {
		win = `C:\Windows`
	}
	a.J.Info(stepDomain, "joining the domain from %s", d.File)
	r := run(5*time.Minute, "djoin", "/requestodj", "/loadfile", blob, "/windowspath", win, "/localos")
	if !r.ok() {
		// A blob is spent the first time it is used and belongs to exactly one
		// machine, so the useful thing to say is which machine and which file,
		// not just the exit code -- whoever reads this is deciding whether to
		// provision another one.
		a.J.FailDetail(stepDomain,
			"this machine did not join the domain — it is usable, but as a workgroup machine, "+
				"and the join must be provisioned again before another attempt",
			trimOut(r.Out))
		a.failed(KindDomain, "domain join", "the machine is in a workgroup; the join must be provisioned again")
		return
	}
	a.done(KindDomain, "domain join")
	a.J.Info(stepDomain, "joined; the domain membership takes effect at the next restart")
	// The join is written into the local security database now and is only
	// real after a restart. Asking for one here means the agent's own restart
	// machinery carries it out between steps and resumes afterwards, so the
	// rest of the build still happens -- on a machine that is by then a
	// domain member.
	a.rebootWanted = "the domain join takes effect after a restart"
}
