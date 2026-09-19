package oscatalog

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
)

// Programs with Ubuntu (docs/plan-linux-apps.md, step 2).
//
// A plain Ubuntu entry writes Ubuntu's ISO as it is, and its installer asks
// everything. Picking programs turns it into an autoinstall: DSKY appends the
// answers (linux.autoinstall), and the installer puts the programs on.
//
// Three decisions shape the answers:
//   - Desktop keeps Ubuntu's own confirmation. The GRUB menu is left alone, so
//     the installer shows "Ready to install — Review your choices" with the
//     programs listed, and waits for Install before it erases the disk. Server
//     is hands-off.
//   - No account or password is in DSKY's answers. On Server the installer
//     asks for the account on screen (interactive identity); on Desktop,
//     Ubuntu's installer asks for it itself.
//   - Unofficial clients are not offered (see appcatalog.UbuntuSource).
//
// apt packages and snaps go in the installer's own sections. Flathub apps and
// Chrome, from Google's repository, install on first boot from a systemd
// service that waits for the network, logs to /var/log/dsky-apps.log, and
// tries again at the next boot if anything failed. That was step 1's lesson
// too: the installer installs snaps on first boot as well.

// ProgramsSupported reports whether picking programs is offered for this
// entry: Ubuntu's own installer, which takes autoinstall answers.
func (e Entry) ProgramsSupported() bool {
	if e.Family == Windows {
		return true
	}
	return e.ubuntuPrograms() || e.kickstartPrograms()
}

// ubuntuPrograms reports whether this entry's installer takes Ubuntu's
// cloud-init autoinstall answers.
func (e Entry) ubuntuPrograms() bool {
	return e.Family == Linux && strings.HasPrefix(e.ID, "ubuntu-") && e.Kind() != ImageRaw
}

// kickstartPrograms reports whether this entry installs through Anaconda and
// so takes a kickstart.
//
// Fedora Server only, for now. The RHEL family -- AlmaLinux, Rocky, RHEL --
// runs the same Anaconda through the same code, and turning each on is one VM
// run each; none of them has had it, and an entry that offers programs it has
// never been watched install is worse than one that offers none. Fedora
// Workstation is a live image, where %packages is not what installs the
// system, so it is a different job rather than one more id here.
func (e Entry) kickstartPrograms() bool {
	return e.Family == Linux && e.ID == "fedora-44-server" && e.Kind() != ImageRaw
}

// ThirdPartyDriversSupported reports whether "drivers for this computer" means
// anything for this entry. It is Ubuntu's question: its installer can fetch the
// proprietary drivers the kernel does not carry when the answers ask it to.
// Anaconda has no equivalent to ask for, so offering the choice on Fedora would
// be a control that changes nothing.
func (e Entry) ThirdPartyDriversSupported() bool { return e.ubuntuPrograms() }

// AppTarget is the operating system the program picker resolves for.
func (e Entry) AppTarget() appcatalog.Target {
	switch {
	case e.Family == Windows:
		return appcatalog.TargetWindows
	case e.kickstartPrograms():
		return appcatalog.TargetFedora
	default:
		return appcatalog.TargetUbuntu
	}
}

// ubuntuDesktop reports whether this is Ubuntu's desktop installer, which
// keeps its confirmation screen.
func (e Entry) ubuntuDesktop() bool { return e.Group() != Server }

// CheckPrograms validates a program list for this entry before anything is
// downloaded.
func CheckPrograms(e Entry, ids []string) error { return checkPrograms(e, ids) }

func checkPrograms(e Entry, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if !e.ProgramsSupported() {
		return fmt.Errorf("installing programs alongside %s is not supported — programs can be installed with Windows, with Ubuntu (Server 24.04 and 26.04, Desktop 26.04) and with Fedora Server", e.Name)
	}
	if err := checkNeedsDesktop(e, ids); err != nil {
		return err
	}
	switch e.AppTarget() {
	case appcatalog.TargetWindows:
		_, _, err := appcatalog.Resolve(ids)
		return err
	case appcatalog.TargetFedora:
		_, err := appcatalog.ResolveFedora(ids)
		return err
	}
	_, err := appcatalog.ResolveUbuntu(ids)
	return err
}

// checkNeedsDesktop refuses a graphical program on a server.
//
// Ubuntu Server and Fedora Server install no desktop at all -- no X, no
// Wayland, no GNOME -- and the picker offered VLC, Chrome and GIMP for them
// anyway. The packages install cleanly, which is the trouble: nothing fails,
// and the machine simply has no way to run any of them. A stick that quietly
// installs something unusable is worse than one that says it will not.
func checkNeedsDesktop(e Entry, ids []string) error {
	if e.Group() != Server {
		return nil
	}
	var refused []string
	for _, id := range ids {
		a, ok := appcatalog.Get(strings.TrimSpace(id))
		if ok && a.Desktop {
			refused = append(refused, a.Name)
		}
	}
	if len(refused) == 0 {
		return nil
	}
	return fmt.Errorf(`%s needs a desktop, and %s installs none.

%s has no graphical session at all, so it would install and then have nothing
to run it. Leave it out, or install a desktop environment yourself afterwards.

For anything the picker will not do, `+"`dsky apps add <installer>`"+` takes your own
installer, and a workspace recipe takes whatever you want to write.`,
		strings.Join(refused, ", "), e.Name, e.Name)
}

// ubuntuUserDataFile is where the generated answers go in a workspace.
func ubuntuUserDataFile(recipeID string) string {
	return "templates/dsky-ubuntu-" + recipeID + ".yaml.tmpl"
}

// writeUbuntuUserData writes the autoinstall answers for a program list into
// the workspace and returns their workspace-relative path.
func writeUbuntuUserData(wsDir, recipeID string, e Entry, ids []string, thirdPartyDrivers bool, acct linuxAccount) (string, error) {
	plan, err := appcatalog.ResolveUbuntu(ids)
	if err != nil {
		return "", err
	}
	rel := ubuntuUserDataFile(recipeID)
	p := filepath.Join(wsDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	// The picker's choices go in as a comment, so the recipe can be opened
	// in the install dialog again: the answers alone only say what Ubuntu
	// installs, not which programs were picked.
	data := strings.Replace(ubuntuUserData(plan, e.ubuntuDesktop(), thirdPartyDrivers, acct), "\n", "\n"+programsMarker+strings.Join(ids, " ")+"\n", 1)
	return rel, os.WriteFile(p, []byte(data), 0o644)
}

// ubuntuUserData renders the cloud-config autoinstall answers. The output is
// also a Go template (compose renders user_data as one), so it must never
// contain "{{".
// linuxAccount is the account an unattended install creates. Password is not
// the password: it is the name of the template variable the hash arrives in,
// so the answers left in a workspace say where to find it and never what it
// is -- the same discipline Windows' admin_password already uses, and for the
// same reason. Empty means no account in the answers, and the installer stops
// to ask for one.
type linuxAccount struct {
	User string
	Hash bool // a hash is supplied at build time
}

const adminHashVar = "${var:admin_password_hash}"

func ubuntuUserData(plan appcatalog.UbuntuPlan, desktop, thirdPartyDrivers bool, acct linuxAccount) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	w("#cloud-config")
	w("# Generated by DSKY. The programs picked in DSKY, for Ubuntu's installer.")
	w("autoinstall:")
	w("  version: 1")
	switch {
	case acct.Hash:
		// An account in the answers, so nothing is asked. The hash arrives
		// through CLIVars at render time and is never written to the
		// workspace: a recipe left behind cannot hand somebody the password.
		w("  identity:")
		w("    hostname: %s", acct.hostname())
		w("    username: %s", acct.User)
		w("    password: \"%s\"", adminHashVar)
	case !desktop:
		// No account in DSKY's answers: the installer asks for it on screen.
		w("  interactive-sections:")
		w("    - identity")
	}
	// Each edition gets the layout its own installer would have chosen, so a
	// machine DSKY built looks like one somebody installed by hand. Ubuntu
	// Server's guided install uses LVM; Ubuntu Desktop's lays down a plain
	// partition. DSKY said "direct" for both, which matched Desktop by
	// accident and quietly gave every server a disk that cannot be extended,
	// snapshotted, or have a second disk added to it later without moving the
	// data off first -- on the edition where that is most likely to be wanted,
	// and with nothing on screen to say a choice had been made at all.
	w("  storage:")
	w("    layout:")
	if desktop {
		w("      name: direct")
	} else {
		w("      name: lvm")
		// sizing-policy: all, because Ubuntu's own default leaves about half
		// the volume group unallocated -- room to grow into or snapshot, which
		// is a sensible thing for a server somebody administers and a puzzle
		// on a machine somebody is handed. A 40 GB disk came back with an
		// 18.5 GB root, and the first person to see that reports it as a
		// fault. The group is still there, so another disk can be added and
		// the volume extended; what is gone is the snapshot headroom.
		w("      sizing-policy: all")
	}
	if thirdPartyDrivers {
		// Ubuntu's own answer to "drivers for this computer". Linux carries
		// its drivers in the kernel, with one exception that matters on a
		// desktop: the proprietary ones, NVIDIA above all. Ubuntu's installer
		// finds and installs those itself when asked, so DSKY asks rather
		// than staging packs the way it must for Windows.
		w("  drivers:")
		w("    install: true")
	}
	if len(plan.Apt) > 0 {
		w("  packages:")
		for _, p := range plan.Apt {
			w("    - %s", p)
		}
	}
	if len(plan.Snaps) > 0 {
		w("  snaps:")
		for _, s := range plan.Snaps {
			w("    - name: %s", s.Name)
			if s.Classic {
				w("      classic: true")
			}
		}
	}
	if plan.FirstBoot() {
		script := base64.StdEncoding.EncodeToString([]byte(ubuntuFirstBootScript(plan)))
		unit := base64.StdEncoding.EncodeToString([]byte(ubuntuFirstBootUnit))
		w("  late-commands:")
		w("    - mkdir -p /target/usr/local/sbin /target/etc/systemd/system")
		w("    - echo %s | base64 -d > /target/usr/local/sbin/dsky-apps.sh", script)
		w("    - chmod 0755 /target/usr/local/sbin/dsky-apps.sh")
		w("    - echo %s | base64 -d > /target/etc/systemd/system/dsky-apps.service", unit)
		w("    - curtin in-target --target=/target -- systemctl enable dsky-apps.service")
	}
	w("  shutdown: reboot")
	return b.String()
}

// ubuntuFirstBootUnit runs the script once per boot until it has nothing left
// to do.
//
// It must not be ordered after cloud-init, and must not wait for it either,
// though both are tempting: cloud-init's last stage is still putting on the
// programs the answers asked for while this runs. Both were tried on Ubuntu
// Server 26.04, and both are worse than the race they fix, because
// cloud-final.service is itself ordered after multi-user.target, which this
// unit is wanted by:
//
//   - After=cloud-final.service closes the loop, and systemd breaks an
//     ordering cycle by deleting a job from it. It deleted this one. The
//     service never ran at all, the programs never arrived, and the only
//     sign of it was one line in the journal.
//   - Waiting for cloud-init from inside the script is the same cycle with
//     the deadlock left in: cloud-final cannot start until multi-user.target
//     is reached, which cannot happen while this service is still running.
//
// So the race is handled where it happens instead: apt is told to wait for
// dpkg's lock rather than fail on it, and each snap is waited for and retried
// rather than declared missing the moment snapd looks idle.
const ubuntuFirstBootUnit = `[Unit]
Description=DSKY: install the programs picked in DSKY
Wants=network-online.target
After=network-online.target
ConditionPathExists=!/var/lib/dsky/apps-done

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/dsky-apps.sh
TimeoutStartSec=0

[Install]
WantedBy=multi-user.target
`

// writeReleaseInstall installs a program published only as a release binary,
// into /usr/local/bin, where it is on every user's PATH rather than one
// account's. The install-to-a-home-directory that the vendor's own script does
// is wrong here: the first boot runs as root, and root's ~/.local/bin is not
// where the person who will use the machine is going to look.
//
// The bytes are checked against the checksum file published in the same
// release before anything is run, and nothing is installed if they do not
// match. The release to take is whichever one is current, resolved by asking
// GitHub what /releases/latest redirects to — no JSON parsing, and no version
// pinned in DSKY that would go stale between DSKY's own releases.
func writeReleaseInstall(w func(string, ...any), rel appcatalog.UbuntuRelease, ensureCurl string) {
	asset := strings.NewReplacer("{tag}", `$tag`, "{arch}", `$arch`).Replace(rel.Asset)
	base := "https://github.com/" + rel.Repo + "/releases"
	w(`if command -v %s >/dev/null 2>&1; then`, rel.Binary)
	w(`  note "%s is installed"`, rel.Name)
	w(`else`)
	w(`  %s >/dev/null 2>&1 || true`, ensureCurl)
	// uname, not dpkg: this same block runs on Fedora, where dpkg does not
	// exist and the arch came out empty -- which made the asset name
	// "dsky-v0.7.41-linux-" and the download a 404 reported as "no build for
	// ". The names releases use are not uname's, so they are mapped.
	w(`  case "$(uname -m)" in`)
	w(`    x86_64) arch=amd64 ;;`)
	w(`    aarch64|arm64) arch=arm64 ;;`)
	w(`    *) arch=$(uname -m) ;;`)
	w(`  esac`)
	w(`  tag=$(curl -fsSLI -o /dev/null -w '%%{url_effective}' %s/latest | sed 's#.*/tag/##')`, base)
	w(`  d=$(mktemp -d)`)
	w(`  if [ -z "$tag" ]; then`)
	w(`    note "FAILED %s: could not find its latest release"; failed=1`, rel.Name)
	w(`  elif ! curl -fsSL -o "$d/bin" "%s/download/$tag/%s"; then`, base, asset)
	w(`    note "FAILED %s: no $tag build for $arch"; failed=1`, rel.Name)
	w(`  elif ! curl -fsSL -o "$d/sums" "%s/download/$tag/%s"; then`, base, rel.Sums)
	w(`    note "FAILED %s: could not fetch its checksums"; failed=1`, rel.Name)
	w(`  else`)
	// The asset name is matched exactly, so a checksum file listing every
	// platform cannot let another platform's binary through.
	w(`    want=$(awk -v a="%s" '$2 == a || $2 == "*" a {print $1}' "$d/sums")`, asset)
	w(`    got=$(sha256sum "$d/bin" | awk '{print $1}')`)
	w(`    if [ -n "$want" ] && [ "$want" = "$got" ]; then`)
	w(`      install -m 0755 "$d/bin" /usr/local/bin/%s`, rel.Binary)
	for _, alias := range rel.Aliases {
		w(`      ln -sf %s /usr/local/bin/%s`, rel.Binary, alias)
	}
	if rel.Exec != "" {
		// A launcher for every account, not root's. Written whether or not a
		// desktop is installed: one that is added later finds it.
		w(`      icon=`)
		if rel.Icon != "" {
			w(`      install -d -m 0755 /usr/local/share/icons`)
			w(`      curl -fsSL -o /usr/local/share/icons/%s "%s/download/$tag/%s" && icon=/usr/local/share/icons/%s`,
				rel.Icon, base, rel.Icon, rel.Icon)
		}
		w(`      install -d -m 0755 /usr/local/share/applications`)
		w(`      cat > /usr/local/share/applications/%s.desktop <<DESKTOP`, rel.ID)
		w(`[Desktop Entry]`)
		w(`Type=Application`)
		w(`Name=%s`, rel.Name)
		w(`Comment=%s`, rel.Comment)
		w(`Exec=/usr/local/bin/%s`, rel.Exec)
		w(`Icon=${icon:-drive-removable-media}`)
		w(`Terminal=false`)
		w(`Categories=System;Utility;`)
		w(`DESKTOP`)
		w(`      update-desktop-database /usr/local/share/applications >/dev/null 2>&1 || true`)
	}
	w(`      note "installed %s $tag"`, rel.Name)
	w(`    else`)
	w(`      note "FAILED %s: the download did not match its published checksum"; failed=1`, rel.Name)
	w(`    fi`)
	w(`  fi`)
	w(`  rm -rf "$d"`)
	w(`fi`)
}

// ubuntuProbeHosts are the hosts this plan has to reach before it is worth
// starting, in the order they are needed. Only what the plan actually uses:
// waiting on a host nothing needs is fifteen minutes of a machine doing
// nothing, once per boot, on any network that happens to block it.
func ubuntuProbeHosts(plan appcatalog.UbuntuPlan) []string {
	var hosts []string
	if len(plan.Apt) > 0 || len(plan.Repos) > 0 || len(plan.Flatpaks) > 0 {
		// Flatpak and every vendor repository are installed with apt's help,
		// so the archive is the first thing any of them needs.
		hosts = append(hosts, "archive.ubuntu.com")
	}
	if len(plan.Snaps) > 0 {
		hosts = append(hosts, "api.snapcraft.io")
	}
	if len(plan.Flatpaks) > 0 {
		hosts = append(hosts, "dl.flathub.org")
	}
	for _, id := range plan.Repos {
		if repo, ok := appcatalog.UbuntuRepoByID(id); ok {
			if h := repoHost(repo.KeyURL); h != "" {
				hosts = append(hosts, h)
			}
		}
	}
	if len(plan.Releases) > 0 {
		hosts = append(hosts, "github.com")
	}
	seen := map[string]bool{}
	out := hosts[:0]
	for _, h := range hosts {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// repoHost is the host part of a repository URL, for the readiness probe.
func repoHost(rawURL string) string {
	rest := rawURL
	for _, scheme := range []string{"https://", "http://"} {
		if s, ok := strings.CutPrefix(rest, scheme); ok {
			rest = s
			break
		}
	}
	host, _, _ := strings.Cut(rest, "/")
	if host == "" || strings.ContainsAny(host, " \t'\"`$") {
		return ""
	}
	return host
}

// ubuntuFirstBootScript installs what the installer's sections can't: Flathub
// apps and vendor-repository packages. It runs until everything succeeds,
// once per boot, and one failure never stops the rest.
func ubuntuFirstBootScript(plan appcatalog.UbuntuPlan) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	w("#!/bin/bash")
	w("# Generated by DSKY. Puts on the programs picked in DSKY that Ubuntu's")
	w("# installer can't — Flathub apps and vendor repositories — and confirms")
	w("# the ones it can. Log: /var/log/dsky-apps.log")
	w("exec >>/var/log/dsky-apps.log 2>&1")
	w(`note() { echo "$(date -Is) $*"; }`)
	w(`note "first-boot programs starting"`)
	w(`export DEBIAN_FRONTEND=noninteractive`)
	// cloud-init's last stage is still installing what the answers asked for
	// while this runs, and it holds dpkg's lock while it does. Waiting for the
	// lock is the whole fix: without it, apt fails immediately with "could not
	// get lock" and a program is reported failed for being early.
	w(`apt() { apt-get -o DPkg::Lock::Timeout=600 "$@"; }`)
	// Waiting for the hosts this plan actually needs, rather than one host
	// that stood for "the internet". A plan with no Chrome in it used to
	// spend fifteen minutes every boot waiting for dl.google.com on a network
	// that blocks Google, and then install everything else perfectly well.
	// One loop over all of them, not one loop each: a loop per host puts the
	// worst case at fifteen minutes times however many programs were picked
	// from different places, and the thing being waited for is the same
	// network in every case.
	if hosts := ubuntuProbeHosts(plan); len(hosts) > 0 {
		w(`for i in $(seq 1 90); do`)
		w(`  ready=1`)
		w(`  for h in %s; do`, strings.Join(hosts, " "))
		w(`    timeout 5 bash -c "</dev/tcp/$h/443" 2>/dev/null || { ready=0; break; }`)
		w(`  done`)
		w(`  [ "$ready" = 1 ] && break`)
		w(`  [ "$i" = 1 ] && note "waiting for the network ($h)"`)
		w(`  sleep 10`)
		w(`done`)
		// Carrying on after the wait runs out is deliberate: a machine behind
		// a proxy that refuses a bare TCP connection can still install
		// everything, and each program says for itself whether it arrived.
		w(`[ "$ready" = 1 ] || note "carrying on without reaching $h; what fails is tried again at the next boot"`)
	}
	w(`failed=0`)
	// apt's own list lock is not DPkg::Lock::Timeout's to hold, so an update
	// racing cloud-init can still lose. It is worth retrying rather than
	// carrying on with a stale list, which is what turns into a package that
	// "does not exist" further down.
	w(`for i in 1 2 3; do`)
	w(`  apt update -q && break`)
	w(`  [ "$i" = 3 ] && note "apt-get update failed"`)
	w(`  sleep 20`)
	w(`done`)

	// What the installer's own sections were asked for, checked on the
	// installed system. A machine whose network came up late, or not at all,
	// finishes its install with those programs quietly missing: the installer
	// does not fail for them and nothing else looks. So the programs are
	// confirmed here and put on if they are not there, which is also what
	// makes trying again at the next boot worth anything.
	// dpkg -s is not the question. It exits 0 for any package dpkg still has
	// a record of, including "install ok half-configured" and "deinstall ok
	// config-files" — which is exactly the state an install cut short by a
	// late network leaves behind, and exactly what this pass exists to find.
	// Only "install ok installed" means the program is there.
	w(`installed() { dpkg-query -W -f='${Status}' "$1" 2>/dev/null | grep -q '^install ok installed$'; }`)
	for _, pkg := range plan.Apt {
		w(`if installed %s; then`, pkg)
		w(`  note "%s is installed"`, pkg)
		w(`elif apt install -y -q %s; then`, pkg)
		w(`  note "installed %s (the installer did not)"`, pkg)
		w(`else`)
		w(`  note "FAILED %s"; failed=1`, pkg)
		w(`fi`)
	}
	if len(plan.Snaps) > 0 {
		// A snap from the snaps: section is not on the disk when the login
		// prompt appears: the installer hands it to snapd, which installs it
		// from the store once the system is up. So this waits for it rather
		// than looking once — and racing it is worse than waiting, because
		// installing a snap that snapd is already installing fails both.
		//
		// The order matters, which is why the unit runs after cloud-final:
		// "snapd has no change in flight" means nothing before cloud-init has
		// asked it for anything, because at that point snapd is idle for want
		// of work rather than for having finished it.
		w(`if command -v snap >/dev/null; then`)
		w(`  snap wait system seed.loaded 2>/dev/null || true`)
		// A grace period before concluding anything: cloud-init asks snapd
		// for these, and until it has, snapd is idle for want of work rather
		// than for having finished, and an install started here races the one
		// about to be started there. Both then fail.
		w(`  grace=18`)
		// One waiter, used for each snap: appear on your own, or be installed.
		w(`  dsky_snap() { # name [--classic]`)
		w(`    local name=$1; shift`)
		w(`    local i`)
		w(`    for i in $(seq 1 60); do`)
		w(`      if snap list "$name" >/dev/null 2>&1; then`)
		w(`        note "$name is installed"; return 0`)
		w(`      fi`)
		w(`      if [ "$i" -le "$grace" ] || snap changes 2>/dev/null | grep -qE '^[0-9]+ +(Do|Doing) '; then`)
		w(`        [ "$i" = 1 ] && note "waiting for $name, which the installer asks snapd for"`)
		w(`        sleep 10; continue`)
		w(`      fi`)
		w(`      if snap install "$@" "$name"; then`)
		w(`        note "installed $name (the installer did not)"; return 0`)
		w(`      fi`)
		w(`      sleep 10`)
		w(`    done`)
		w(`    note "FAILED $name"; failed=1; return 1`)
		w(`  }`)
		for _, s := range plan.Snaps {
			if s.Classic {
				w(`  dsky_snap %s --classic`, s.Name)
			} else {
				w(`  dsky_snap %s`, s.Name)
			}
		}
		w(`else`)
		w(`  note "FAILED: snapd is not on this system, so the snaps cannot be installed"; failed=1`)
		w(`fi`)
	}
	for _, id := range plan.Repos {
		repo, ok := appcatalog.UbuntuRepoByID(id)
		if !ok {
			// A program naming a repository DSKY has no recipe for would
			// otherwise install nothing and say nothing about it.
			w(`note "FAILED %s: DSKY does not know this repository"; failed=1`, id)
			continue
		}
		arch := ""
		if repo.AMD64Only {
			arch = "arch=amd64 "
		}
		keyring := "/etc/apt/keyrings/" + repo.ID + ".asc"
		w(`if installed %s; then`, repo.Package)
		w(`  note "%s already installed"`, repo.Name)
		if repo.AMD64Only {
			w(`elif [ "$(dpkg --print-architecture)" != amd64 ]; then`)
			w(`  note "SKIPPED %s: the vendor only publishes it for amd64"`, repo.Name)
		}
		w(`else`)
		w(`  install -d -m 0755 /etc/apt/keyrings`)
		w(`  if apt install -y -q curl && curl -fsSL %s -o %s; then`, repo.KeyURL, keyring)
		w(`    echo "deb [%ssigned-by=%s] %s %s %s" > /etc/apt/sources.list.d/%s.list`,
			arch, keyring, repo.URL, repo.Suite, repo.Comps, repo.ID)
		w(`    for i in 1 2 3; do apt update -q && break; sleep 20; done`)
		w(`    if apt install -y -q %s; then note "installed %s"; else note "FAILED %s"; failed=1; fi`,
			repo.Package, repo.Name, repo.Name)
		w(`  else`)
		w(`    note "FAILED %s: could not fetch the vendor's signing key"; failed=1`, repo.Name)
		w(`  fi`)
		w(`fi`)
	}
	if len(plan.Flatpaks) > 0 {
		w(`if ! command -v flatpak >/dev/null; then apt install -y -q flatpak || { note "FAILED installing flatpak"; failed=1; }; fi`)
		w(`if command -v flatpak >/dev/null; then`)
		w(`  flatpak remote-add --system --if-not-exists flathub https://dl.flathub.org/repo/flathub.flatpakrepo || { note "FAILED adding Flathub"; failed=1; }`)
		for _, id := range plan.Flatpaks {
			w(`  if flatpak install --system -y --noninteractive flathub %s; then note "installed %s"; else note "FAILED %s"; failed=1; fi`, id, id, id)
		}
		w(`fi`)
	}
	for _, id := range plan.Releases {
		rel, ok := appcatalog.UbuntuReleaseByID(id)
		if !ok {
			w(`note "FAILED %s: DSKY does not know this release"; failed=1`, id)
			continue
		}
		writeReleaseInstall(w, rel, "apt install -y -q curl ca-certificates")
	}
	w(`if [ "$failed" = 0 ]; then`)
	w(`  mkdir -p /var/lib/dsky && touch /var/lib/dsky/apps-done`)
	w(`  note "first-boot programs done"`)
	w(`else`)
	w(`  note "first-boot programs finished with failures; trying again at the next boot"`)
	w(`fi`)
	w(`exit 0`)
	return b.String()
}

// hostname is what the machine calls itself when DSKY supplies the account.
// Derived from the user name rather than asked for separately: a third
// question to answer before building a stick, to set something the owner can
// change in a menu, is not worth the width it would take on the screen.
func (a linuxAccount) hostname() string {
	h := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		}
		return -1
	}, a.User)
	if h == "" {
		return "dsky"
	}
	return h + "-pc"
}
