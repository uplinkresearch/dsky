package oscatalog

import (
	"strings"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
	"github.com/uplinkresearch/dsky/internal/recipe"
)

// What a stick does when somebody boots it.
//
// Three surfaces used to work this out for themselves from raw fields — the
// CLI asked "is this Windows?", the portal's recipe page asked "is there an
// autoinstall section?", and the picker in index.html had a flag called
// `ubuntu` that meant "any Linux". Every one of them was a branch somebody had
// to remember to add, and when the kickstart path arrived none of them got it:
// `dsky install fedora-44-server --apps docker` announced that the stick would
// install Ubuntu and fetch docker.io, and the recipe page called a kickstart
// that runs `clearpart --all` "Boots the installer — nothing set in advance".
//
// So the question is answered once, here, and the surfaces render the answer.
// A new way of installing is then a new case in one switch, and the test below
// walks the whole catalog and fails if any entry comes back describing itself
// as something it is not.
type Automation int

const (
	// BootsInstaller: DSKY wrote no answers. The ISO is written as published
	// and whoever boots it does the install.
	BootsInstaller Automation = iota
	// ConfirmsOnScreen: the answers are there and the installer reads them,
	// but it still stops and waits for somebody to press Install. Ubuntu's
	// desktop installer, whose prompt DSKY deliberately leaves alone.
	ConfirmsOnScreen
	// Unattended: nobody at the keyboard. The disk goes.
	Unattended
)

// InstallPlan is what a stick built from an entry (or held in a recipe) will
// do. Facts, not sentences: each surface has its own voice, and the phrases
// below are offered for the ones that want to agree.
type InstallPlan struct {
	// OSName is the entry's own name — "Fedora 44 Server", never the family
	// of whichever answer file happens to be involved.
	OSName     string
	Automation Automation
	// ErasesDisk is true whenever DSKY's answers partition the disk, which is
	// every case where answers are written at all.
	ErasesDisk bool
	// AsksForAccount: the one stop. Neither Ubuntu's identity section nor
	// DSKY's kickstart carries a password, by an old decision to keep them out
	// of DSKY, so both installers pause once to ask for one.
	AsksForAccount bool
	// Programs, grouped by where they come from, in the order they happen.
	Programs []ProgramGroup
	// ThirdPartyDrivers: the installer was asked to find and install the
	// proprietary drivers for this machine. Ubuntu only.
	ThirdPartyDrivers bool
}

// ProgramGroup is one source and what comes from it.
type ProgramGroup struct {
	Where string
	Names []string
}

// InstallPlan describes what a stick built from this entry with these options
// will do, before it is built.
func (e Entry) InstallPlan(opts Options) InstallPlan {
	p := InstallPlan{OSName: e.Name}
	// The same condition oscatalog uses to decide whether to write answers at
	// all: no answers, no automation, whatever else was asked for.
	if e.Family != Linux || !e.ProgramsSupported() || (len(opts.Apps) == 0 && !opts.ThirdPartyDrivers) {
		return p
	}
	p.ErasesDisk = true
	p.AsksForAccount = true
	p.ThirdPartyDrivers = opts.ThirdPartyDrivers && e.ThirdPartyDriversSupported()
	// Ubuntu's desktop installer keeps its review screen — DSKY does not patch
	// its GRUB menu — so it reads the answers and then waits to be told.
	if e.AppTarget() == appcatalog.TargetUbuntu && e.ubuntuDesktop() {
		p.Automation = ConfirmsOnScreen
	} else {
		p.Automation = Unattended
	}
	p.Programs = programGroups(e.AppTarget(), opts.Apps)
	return p
}

// RecipeInstallPlan describes what a saved recipe will do. The recipe is the
// truth here rather than the options, because it may have been edited as a
// file since the dialog wrote it.
func RecipeInstallPlan(rc *recipe.Recipe, form *RecipeForm) InstallPlan {
	p := InstallPlan{OSName: rc.OS.Source}
	e, known := Get(rc.OS.Source)
	if known {
		p.OSName = e.Name
	}
	if rc.Linux == nil {
		return p
	}
	switch {
	case rc.Linux.Autoinstall != nil:
		p.ErasesDisk, p.AsksForAccount = true, true
		// PatchKernel is how an autoinstall recipe records whether Ubuntu's
		// own confirmation was left in place.
		if rc.Linux.Autoinstall.PatchKernel() {
			p.Automation = Unattended
		} else {
			p.Automation = ConfirmsOnScreen
		}
	case rc.Linux.Kickstart != nil:
		// A kickstart carries clearpart and autopart and asks nothing but the
		// account. This is the case all three surfaces had forgotten.
		p.ErasesDisk, p.AsksForAccount, p.Automation = true, true, Unattended
	default:
		return p
	}
	// The program list and the drivers flag are not in the recipe: they live
	// in the answers file, and the form is what has already read them back.
	if known && form != nil {
		p.Programs = programGroups(e.AppTarget(), form.Apps)
		p.ThirdPartyDrivers = form.ThirdPartyDrivers && e.ThirdPartyDriversSupported()
	}
	return p
}

// programGroups resolves ids against the table the installer will actually
// read. An id is not a package: `docker` is docker.io on Ubuntu and
// moby-engine on Fedora, which is the whole reason there are two tables.
func programGroups(target appcatalog.Target, ids []string) []ProgramGroup {
	if len(ids) == 0 {
		return nil
	}
	var groups []ProgramGroup
	add := func(where string, names ...string) {
		if len(names) > 0 {
			groups = append(groups, ProgramGroup{Where: where, Names: names})
		}
	}
	var flatpaks, repos, releases []string
	if target == appcatalog.TargetFedora {
		plan, err := appcatalog.ResolveFedora(ids)
		if err != nil {
			return nil
		}
		add("from Fedora", plan.Dnf...)
		flatpaks, repos, releases = plan.Flatpaks, plan.Repos, plan.Releases
	} else {
		plan, err := appcatalog.ResolveUbuntu(ids)
		if err != nil {
			return nil
		}
		add("from Ubuntu", plan.Apt...)
		for _, sn := range plan.Snaps {
			add("snap", sn.Name)
		}
		flatpaks, repos, releases = plan.Flatpaks, plan.Repos, plan.Releases
	}
	add("Flathub, on first boot", flatpaks...)
	add("vendor repository, on first boot", repos...)
	add("published release, on first boot", releases...)
	return groups
}

// Any reports whether any program is installed at all.
func (p InstallPlan) Any() bool { return len(p.Programs) > 0 }

// InstallerLine is the one-line answer to "what happens when this boots?",
// in the words the portal's recipe page uses.
func (p InstallPlan) InstallerLine() string {
	switch p.Automation {
	case Unattended:
		return "Installs by itself and erases the computer's disk"
	case ConfirmsOnScreen:
		return "Waits for Install on the review screen, then erases the computer's disk"
	default:
		return "Boots the installer — nothing set in advance"
	}
}

// StopsForLine is what the installer will interrupt itself to ask, or "" when
// it asks nothing because it was never given answers.
func (p InstallPlan) StopsForLine() string {
	switch {
	case p.Automation == ConfirmsOnScreen:
		// Not the OS name: this sentence follows one that has just used it,
		// and "Ubuntu 26.04 LTS Desktop shows its review screen" after
		// "install Ubuntu 26.04 LTS Desktop by itself" reads like a stutter.
		return "The installer shows its review screen and waits for Install before erasing anything."
	case p.AsksForAccount:
		return "No questions except the account, which the installer asks for on screen."
	default:
		return ""
	}
}

// PickerNote is the sentence the program picker shows once something is
// ticked. Built here so the page does not have to assemble it from a flag
// called `ubuntu` that means "any Linux".
func (p InstallPlan) PickerNote() string {
	var b strings.Builder
	b.WriteString("Picking programs makes the stick install ")
	b.WriteString(p.OSName)
	b.WriteString(" by itself and erase the computer's disk. The programs install during setup and on first boot, so the computer needs to be online. ")
	b.WriteString(p.StopsForLine())
	return b.String()
}
