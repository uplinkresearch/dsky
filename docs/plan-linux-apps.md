# Plan: installing programs with Linux

Decisions marked **Decide** are Dusty's.

**Step 1 done (2026-09-14):** DSKY's autoinstall media, built from the real
ISOs, installed Ubuntu in a KVM virtual machine (`ubuntu-autoinstall` workflow,
run 34855847949):

| Case | Result |
|---|---|
| Server 24.04, zero-touch | Installed with no input in 4 min; answers, apt package, account, late-command and snap all present; booted |
| Server 26.04, zero-touch | Same, 5 min |
| Desktop 26.04, zero-touch | Same, 14 min; booted to the desktop |
| Desktop 26.04, prompt kept, no account | The installer read DSKY's answers and stopped on "Ready to install — Review your choices", listing them, with an Install button: the one confirmation before erasing |

Two things learned for step 2: snaps listed in the answers are installed on
first boot, not during install; and releases.ubuntu.com can be too slow to rely
on, which is why DSKY now downloads Ubuntu from the fastest mirror (v0.7.15).
Still to see: what the Desktop installer asks after Install when the answers
carry no account.

**Decided (2026-09-14):** keep Ubuntu's "Continue with autoinstall?" prompt on
Desktop (skip it on Server); leave unofficial clients out; keep passwords out
of DSKY for now, so the account is not in DSKY's answers. **Step 1** (booting
real Ubuntu ISOs in a VM) is built as `test/autoinstall/vm.sh` and the
`ubuntu-autoinstall` workflow; nothing after it is.

**Step 2 done (2026-09-17):** programs install with Ubuntu, from all four
sources, proven in VMs on this machine. See "What step 2 turned out to be"
below — most of the work was not the program table.

## Where it is today

Steps 1 to 3 are built and proven. Ubuntu takes a program list through the
same picker Windows uses, in all three ways in (CLI, terminal wizard, portal),
and `--drivers` means something on Ubuntu too.

Steps 4 and 5 — kickstart for the Fedora/RHEL family, and the after-install
script for distros with a live installer — are as they were.

## What step 2 turned out to be

The program table was the easy half. Every bug worth the name was in *when*
things happen on a machine that has just installed itself, and none of them
would have been found by reading the code — each was watched happening in a
VM.

**The snaps are not there when the login prompt is.** A snap in the answers'
`snaps:` section is an ordinary store install that cloud-init asks snapd for
once the system is up. DSKY's first-boot pass checked for one, did not find
it, saw snapd with nothing in flight, concluded the installer had not
installed it, and installed it itself — 13 seconds into the first boot, which
is before cloud-init has asked snapd for anything at all. The install failed,
the log said `FAILED brave`, and then the installer's own request installed
Brave a minute later. An idle snapd means "not asked yet" as often as it means
"finished".

**Waiting for cloud-init deadlocks the boot, and ordering after it is worse.**
The obvious fix — have the first-boot service wait for cloud-init to finish —
hangs: `cloud-final.service` is itself ordered after `multi-user.target`, which
the service is wanted by, so it cannot start until the service it is waiting
for can start. Fifteen minutes of nothing, then a timeout.

Declaring `After=cloud-final.service` instead looks like the correct systemd
way to say it and is the most dangerous of the three. It closes an ordering
cycle, and systemd resolves a cycle by deleting a job from it. It deleted this
one:

    multi-user.target: Found ordering cycle: dsky-apps.service/start after
    cloud-final.service/start after multi-user.target/start - after
    dsky-apps.service
    multi-user.target: Job dsky-apps.service/start deleted to break ordering
    cycle starting with multi-user.target/start

The service never ran, no programs arrived, nothing failed, and the only trace
was that line in the journal. A test that asked "did the install succeed" would
have passed.

So the races are handled where they happen rather than by ordering around them:
apt is given `DPkg::Lock::Timeout` so it waits for the lock cloud-init is
holding instead of failing on it, and each snap is waited for — with a grace
period before DSKY will believe it is missing — and retried, rather than
declared absent the first time snapd looks idle. `ubuntuFirstBootUnit` carries
the whole story, because the next person to read that unit will think of
exactly the same two "fixes".

**Enter does not press Install.** On Desktop, Ubuntu's review screen puts the
keyboard focus in the scrollable list of answers, which takes Enter for itself.
The run that "pressed Install" sat there until it timed out, with screenshots
that looked like a hung installer. Tab, then Enter.

**The harness stopped needing root.** It checked the installed system by
attaching the disk with `qemu-nbd` and mounting it, which needs root, a kernel
module and a spare device node. It now boots the installed system and asks it,
over SSH, with a key the test's own answers carry. That runs on a workstation
with no sudo at all, it checks the running system rather than files on a disk,
and it is faster: the first check lands 15 seconds after the installer reboots.

## How Linux installers can be given a program list

The 27 entries fall into four groups, and each takes answers differently.

| Group | Entries | How answers get in | Programs possible |
|---|---|---|---|
| Ubuntu installer (subiquity) | Ubuntu Server 24.04 and 26.04, Ubuntu Desktop 26.04 | cloud-init `autoinstall`: DSKY already builds this | Yes, during install |
| Anaconda with kickstart | Fedora Server 44, AlmaLinux 10, Rocky 10, RHEL 10 | Anaconda looks for a volume labelled **OEMDRV** holding `ks.cfg`, with no boot option needed. DSKY could append that partition the way it appends CIDATA | Yes, during install |
| Live desktops with their own installer | Fedora Workstation, Linux Mint, Pop!_OS, CachyOS, Garuda, Nobara, PikaOS, openSUSE, Bazzite | None that works unattended | Only afterwards, by a script run from the stick |
| No fit | Arch (installer shell), NixOS (declarative), Omarchy, SteamOS recovery, Raspberry Pi OS, Proxmox VE, TrueNAS SCALE, Debian netinst | Various, or not a desktop at all | Not planned (Debian could use preseed later) |

## 1. Ubuntu first

Ubuntu is the recommendation because the hard part, getting answers into the
installer, is already written.

### Prove the path on a real ISO

Before any picker work, build a stick image from the real Ubuntu Server 26.04
ISO with DSKY's autoinstall, and boot it in a virtual machine:

- **Where:** a GitHub workflow, since GitHub's Linux runners provide KVM, or on
  kessel after `pacman -S qemu-full edk2-ovmf`.
- **Pass means:** the VM installs with no input, reboots, and a check over
  serial console or SSH finds `/etc/dsky-provisioned` and the packages.
- **Then Ubuntu Desktop 26.04,** whose installer is newer and whose GRUB menu
  is different. Whether the same same-length GRUB rewrite works there is
  unknown until it boots.

Nothing touches a real disk. If either ISO fails, fixing that comes before
programs.

### Where each program comes from

Ubuntu has three sources. The rule, in order:

1. **Ubuntu's own archive (apt)** when it has the program. It comes from
   Ubuntu and is updated with the system.
2. **Snap Store** when the publisher is the vendor (Snapcraft marks it
   *verified*) or Snapcraft's own team (*starred*). Ubuntu installs snaps
   natively, and the installer has a `snaps:` section for them.
3. **Flathub** for the rest, preferring publisher-verified apps. Ubuntu does
   not ship Flatpak, so the first one adds `flatpak` and the Flathub remote.
4. **The vendor's own apt repository** only where that is the vendor's
   official channel and nothing above is: Google Chrome, AnyDesk, TeamViewer.
5. **A published release binary**, for a program packaged nowhere at all,
   fetched and checked against the checksum file published beside it. Last,
   because nothing updates it with the system afterwards. DSKY is the only
   one: it installs to `/usr/local/bin`, not to the root account's home the
   way its own `curl … | sh` installer would at first boot.

Checked on 2026-09-14 against packages.ubuntu.com (noble = 24.04,
resolute = 26.04), the Snapcraft store API and the Flathub API, and re-checked
on 2026-09-17 — this time by a test rather than by hand
(`TestUbuntuNamesLive`, run weekly by catalog-health), which also refuses a
Flathub app whose publisher verification has lapsed. AnyDesk and TeamViewer
came in on the 17th: both were left out for having no verified Flathub app,
which is true and beside the point, because neither vendor ships through
Flathub at all — each runs its own apt repository, which is the rule Chrome
comes in under.

| Program | Ubuntu source | Notes |
|---|---|---|
| Firefox, Thunderbird, LibreOffice | snap (Canonical/Mozilla, verified) | On Ubuntu the apt names install the snap anyway |
| VLC, GIMP, Inkscape, Blender, Audacity, OBS Studio, HandBrake, calibre, KeePassXC, BleachBit, qBittorrent, Remmina (for Remote Desktop) | apt | All in both 24.04 and 26.04 |
| Wireshark, Nmap, WireGuard, OpenVPN, PuTTY, 7-Zip (`7zip`), Git, Python 3, Node.js, Docker (`docker.io`), Java 21 (`openjdk-21-jre`), Steam (`steam-installer`) | apt | Both releases |
| Brave, Opera, Vivaldi, Slack, Telegram, ONLYOFFICE, Bitwarden, Spotify, Plex, Postman, Tailscale | snap, verified publisher | |
| Discord, Signal | snap, Snapcrafters (starred) | Also on Flathub |
| VS Code, PowerShell | snap, verified, **classic** confinement | Installer needs `classic: true` |
| Obsidian | Flathub (verified) | Its snap is classic and unverified |
| LibreWolf, 1Password, Epic Games and GOG (both through Heroic Games Launcher) | Flathub, verified | |
| Zoom, Dropbox, GitHub Desktop, Zotero | Flathub, unverified | Zoom and Zotero snaps are unofficial too |
| Google Chrome, AnyDesk, TeamViewer | the vendor's own apt repository | Each is the vendor's own Linux channel; the Flathub Chrome is an unverified wrapper |
| Microsoft Teams | none official | Only "Teams for Linux", an unofficial client: leave out, or label it |
| Notion | none official | Unofficial snap only: leave out |

**Not on Linux, so hidden when the OS is Linux:** Microsoft 365 Apps, Webex,
OneDrive, Google Drive, Box, Malwarebytes,
PowerToys, Notepad++, ShareX, Everything, Flow Launcher, Sysinternals, WizTree,
WinDirStat, Rufus, CPU-Z, HWMonitor, HWiNFO, CrystalDiskInfo, WinMerge,
mRemoteNG, WinSCP, Paint.NET, IrfanView, K-Lite, Greenshot, VC++ and .NET
runtimes, Oracle Java, Windows Terminal, JetBrains Toolbox, EA app, Ubisoft
Connect, Nmap's Windows build. The picker shows only what the chosen OS can
install, so nothing Windows-only is offered for Ubuntu.

In code, each `App` gains `Snap` (with a classic flag), `Flatpak`, and uses its
existing `Apt`, and `InstallsOn(os)` replaces `InstallsOnWindows`.

### When they install, and the log

- **apt packages and snaps:** in the installer's own `packages:` and `snaps:`
  sections, so they are there on first boot. Both need the network during
  install. The server installer already expects it, and the desktop installer
  needs to be confirmed to.
- **Flatpaks and Chrome:** on first boot, from the installed system's
  cloud-init, retrying until online, because they are larger and more likely
  to fail on a slow link.
- **Log:** everything is logged to `/var/log/dsky-apps.log`, and one failure
  never stops the rest, the same as `apps.ps1` on Windows.

### What the Install dialog gains for Ubuntu

The same program picker, filtered to Linux. Autoinstall also has to be told
what Windows' local-account option covers today:

- **Computer name, user name and password.** Autoinstall needs the password as
  a SHA-512 crypt hash. DSKY computes that: a small pure-Go implementation, or
  a small dependency.
- **Disk:** autoinstall erases the first disk without asking, the same as
  DSKY's Windows sticks. The dialog says so, like the Windows entries' firmware
  notes.

**Decide:**

1. **Hands-off or one prompt:** the rewritten GRUB boots straight into the
   install, or DSKY leaves Ubuntu's "Continue with autoinstall?" question in,
   so a stick booted on the wrong machine stops once before erasing it.
   Recommended: keep the question on Desktop, skip it on Server.
2. **Unofficial clients** (Teams for Linux, Notion snap, Zoom snap): leave out,
   or include with an "unofficial" label.
3. **Account:** type a password in the dialog, or have Ubuntu ask for the
   account on first boot (the `identity` section left interactive). The second
   keeps passwords out of DSKY but means one stop at the keyboard.

**Step 4's gate passed (2026-09-17), and the mechanism is not the one below.**
Fedora Server 44 now installs unattended from a kickstart DSKY appends to the
ISO — 3 minutes, no questions, `%packages` honoured, `%post` run
(`fedora-44-kickstart`). But almost nothing in the sketch below survived
contact with a real Anaconda ISO:

- **"No boot option needed" is false.** Fedora ships `set default="1"`, and
  entry 1 is "Test this media & install". That media check hashes the whole
  device, which now has an answers partition appended to it, so it fails —
  "It is not recommended to use this media" — and halts before the installer
  starts. The menu has to be rewritten, so a boot option costs nothing extra.
- **The menu rewrite has to keep the ISO's `search --set=root` line.** Every
  path in the entry is relative to `$root`, and on an Anaconda ISO `$root` is
  set by searching for the volume label. Drop it and GRUB shows the menu,
  fails to find the kernel and falls back to the menu — which on screen is
  indistinguishable from a timeout that never fired. Two runs to tell apart.
- **OEMDRV cannot work here, and not because of the label.** The ISO9660
  volume descriptor sits at offset 0 of the image, so the ISO's volume label
  belongs to the *whole disk* as well as to the partition holding it.
  `inst.stage2=hd:LABEL=<iso label>` therefore resolves to `/dev/sda` and
  mounts it; with the whole disk mounted, no partition on that disk can be
  opened exclusively, and the kickstart fetch fails with "Can't open blockdev"
  over a partition that is present, correctly labelled and perfectly good.

So the answers ride in as a **second initramfs** instead. The kernel
concatenates every initramfs it is given, GRUB can load one from the appended
partition, and `inst.ks=file:/ks.cfg` reads it from the initramfs root before
any disk is touched. Nothing is mounted, so nothing can be busy.
`internal/compose/cpio.go` carries the whole account.

Two smaller things, for whoever does the RHEL family next: the answers
partition is labelled `DSKYKS`, not `OEMDRV`, because OEMDRV is also how
Anaconda recognises a *driver disk*; and a `%packages` entry that is not on the
media fails the entire install with "No match for argument", so the names have
to be checked against the ISO rather than assumed.

**Step 4 built (2026-09-17).** Picking programs for Fedora Server writes a
kickstart the same way picking them for Ubuntu writes autoinstall answers, and
`dsky apps --os fedora` says where each one comes from. Two things about the
Fedora table are decisions rather than omissions:

- **The table is not a translation of the Ubuntu one.** Checked against Fedora
  44's own metadata: `p7zip` is `7zip`, Node.js is packaged per major version
  (`nodejs24`), and `telegram-desktop`, `handbrake` and `steam` are not in
  Fedora's repositories at all. Where Flathub has a publisher-verified app it
  takes over — Fedora is comfortable with Flatpak in a way Ubuntu is not — and
  where it does not, the program is simply not offered.
- **RPM Fusion is not enabled.** It would add Steam and the codec packages in
  one line, and it changes where the whole machine gets updates from. That is
  the owner's decision, not a side effect of ticking a box in a picker.

`%packages --ignoremissing` is load-bearing and was learned the hard way:
install media carries a subset of the archive (the Server DVD has
`vim-enhanced` and not `vlc`), `%packages` resolves against the media, and one
name it happens not to carry ends the whole install with "No match for
argument" — after the disk has been erased. With it, what the medium has goes
on during setup and the rest is left to the first-boot pass, which installs
from Fedora's own repositories and logs which ones it had to fetch.

Still only Fedora Server. AlmaLinux, Rocky and RHEL run the same Anaconda
through the same code, and turning each on is one VM run each; until one has
had it, offering programs there would be offering something never watched
install.

**Step 4 done (2026-09-17):** the program picker works on Fedora Server, with
one program from each source it has, checked on the installed machine
(`fedora-44-programs`):

    installed vlc (the installer did not)      Fedora's own repositories
    installed Google Chrome                    a vendor's rpm repository
    installed md.obsidian.Obsidian             Flathub
    installed DSKY v0.7.41                     a published release
    first-boot programs done

Three things worth knowing before the RHEL family is turned on:

- **`%packages` needs `--ignoremissing`.** Install media carries a subset of
  the archive — the Server DVD has `vim-enhanced` and not `vlc` — and
  `%packages` resolves against the media, so one name it happens not to carry
  ends the whole install with "No match for argument", after the disk has been
  erased. With the flag, what the media has goes on during setup and the rest
  is left to the first-boot pass, which installs from Fedora's repositories and
  logs which ones it had to fetch. That is why `vlc` reads "the installer did
  not" above.
- **The tables are not translations of each other.** Fedora ships no Steam and
  no codec packages, packages Node.js by major version, and several publishers
  Ubuntu gets from verified snaps are unverified on Flathub. DSKY does not
  enable RPM Fusion to paper over it: that changes what the whole machine gets
  updates from, which is the machine owner's decision. Where a program has no
  acceptable source it is not offered, and the picker says so by not listing
  it — `dsky apps` now marks each program "Windows only", "Ubuntu and Fedora
  only", and so on.
- **Anything shared between the two has to be portable.** The release installer
  asked `dpkg --print-architecture`, which does not exist on Fedora, so the
  asset name came out `dsky-v0.7.41-linux-` and the download 404'd — reported
  as "no build for ", with nothing after "for".

**The RHEL family installs too, and still does not offer programs
(2026-09-17).** AlmaLinux 10.2 and Rocky 10.2, both minimal images, install
unattended from the same kickstart path — 3 and 4 minutes, `%post` run,
`%packages` honoured, booted (`almalinux-10-kickstart`, `rocky-10-kickstart`).
With Fedora Server that is every Anaconda entry in the catalog but RHEL itself,
which is import-only and needs a Red Hat account to get at. One difference in
the media, and one reason the picker stays off:

- **The rebuilds spell it `linuxefi` and `initrdefi`** where Fedora writes
  `linux` and `initrd`. A menu rewrite that assumes Fedora's spelling finds
  nothing and the build stops before writing anything, which is at least a
  loud failure. The replacement now writes back whichever the ISO used.
- **Fedora's package names are mostly not RHEL's.** Of the 25 dnf names in
  DSKY's Fedora table, 8 exist in AlmaLinux 10's BaseOS, AppStream and CRB
  together: firefox, thunderbird, wireguard-tools, wireshark, nmap, git,
  python3 and nodejs24. Not there: libreoffice, calibre, keepassxc, vlc, gimp,
  obs-studio, audacity, inkscape, blender, openvpn, remmina, bleachbit, 7zip,
  qbittorrent, putty, tailscale and moby-engine. Sharing the table would offer
  seventeen programs that cannot install.

So `kickstartPrograms()` is still Fedora Server alone. **Decide** what the RHEL
family should be offered, given the above:

1. **Flathub for the desktop programs.** `flatpak` *is* in AlmaLinux's own
   AppStream, and the verified-publisher rule needs no change, so this works
   today with a third table and no third-party repository. It does mean a
   minimal server image growing a Flatpak stack, which may be the wrong shape
   for the machines these images are usually for.
2. **EPEL.** The Fedora project's own companion repository for RHEL. Far less
   invasive than RPM Fusion and near-universal in RHEL shops, but still a
   repository DSKY would be turning on for the whole machine.
3. **The eight, and nothing else.** Honest, tiny, and arguably right for a
   minimal server install — the picker would simply be short.

### What each option is actually worth (measured 2026-09-18)

"EPEL is where most of the seventeen live" was an assumption, and it is wrong.
Counted against the real repodata — AlmaLinux 10 BaseOS, AppStream, CRB and
extras (6,944 names) and EPEL 10 Everything (25,879) — against the 25 dnf names
`dsky apps --os fedora` actually offers:

| Source | Adds | Running total of 25 |
| --- | --- | --- |
| AlmaLinux's own repositories | 8 | 8 |
| \+ EPEL | 6 | 14 |
| \+ Flathub, verified publishers only | 4 | 18 |
| \+ vendor rpm repositories | 2 | **20** |

- **EPEL adds six, not "most":** 7zip, keepassxc, openvpn, qbittorrent,
  remmina, vlc. It does not carry a single one of the desktop programs people
  ask for by name.
- **Flathub covers exactly the ones EPEL misses**, and four are verified:
  LibreOffice, GIMP, Inkscape, OBS Studio. That is the gap the desktop cares
  about, and it needs no repository DSKY has to trust.
- **Two more need no new mechanism at all.** Tailscale and Docker publish
  RHEL 10 repositories (`pkgs.tailscale.com/stable/rhel/10`,
  `download.docker.com/linux/rhel`), and DSKY already installs from vendor rpm
  repositories — it is how Chrome, AnyDesk and TeamViewer arrive. Neither was
  considered above.
- **Five cannot be reached honestly.** Blender, Audacity, calibre and BleachBit
  are on Flathub but *unverified*, and the verified-publisher rule is worth
  more than four entries; PuTTY has no Flathub app at all.

So the real choice is narrower than it looked: Flathub plus the two vendor
repositories reaches **14 of 25 with no EPEL**, and EPEL is worth six utilities
on top. Option 1 is not the small option — it is most of the value.

Nothing here is blocked on that decision: the kickstart path is proven for the
family either way, and a workspace recipe with `linux.kickstart` can already
install any of them unattended with whatever `%packages` the operator names.

## 2. Fedora Server, AlmaLinux, Rocky, RHEL: kickstart

The same idea with a different answer file, reusing the picker and the
program table:

- **The partition:** DSKY appends a small FAT partition labelled `OEMDRV` with
  `ks.cfg`, which Anaconda finds by itself.
- **The answer file:** its `%packages` installs dnf packages, and a `%post`
  section adds Flathub for the rest.
- **The table:** each program also needs a dnf name. Most of the apt names
  above match, and EPEL is needed for some on Alma and Rocky.
- **Prove first:** that the OEMDRV partition is picked up from the same stick
  the installer booted from, in a VM, before anything else here.

## 3. Everything with a live installer: a script on the stick

Mint, Pop!_OS, Fedora Workstation, CachyOS and the rest can't take answers.
What they can take is a script:

- **The partition:** DSKY appends a small `DSKY` partition holding
  `install-programs.sh` and the picks.
- **What the script does:** after installing the OS, plug the stick in and run
  it. It finds the package manager (apt, dnf, pacman, zypper), installs what
  the distro has, and uses Flathub for the rest. Bazzite and other atomic
  systems get Flatpak only.
- **Not hands-off:** it's one command, but it covers every distro.
- **Prove first:** appending a partition to a hybrid ISO has to be proven not
  to stop each ISO booting. Some are built with isohybrid MBR-only layouts,
  which the CIDATA code has never handled.

## 4. Keep the names honest

Extend the weekly catalog-health run, which already checks OS links and winget
IDs, to confirm every apt, snap and Flathub name in the table still exists.
Snap and Flathub have public APIs that answer 200 or 404, and apt names are
checked against packages.ubuntu.com for each supported release.

## Order and size

1. **Boot real Ubuntu Server and Desktop ISOs with autoinstall in a VM** (a
   day, most of it the workflow). Everything else waits on this.
2. **Ubuntu programs:** table in code, `InstallsOn(os)`, autoinstall sections,
   first-boot Flatpak script, dialog fields, VM check that programs arrived (2
   to 3 days).
3. **Weekly name check** (a couple of hours).
4. **Kickstart for Fedora Server and the RHEL family** (2 days, after proving
   OEMDRV in a VM).
5. **The after-install script for live-installer distros** (2 days, mostly
   proving each ISO still boots with a partition appended).

1 to 3 fit one release; 4 and 5 are separate.

## Not in this plan

- Arch, NixOS and Omarchy: each has its own install tool
  (`archinstall --config`, a Nix configuration), and a picker would fight it.
  Confirmed for Omarchy on 2026-09-17 by booting it (`omarchy` case): the ISO
  is archiso with `timeout=0` and a hidden menu, so it goes straight into
  Omarchy's own installer, which asks for a keyboard layout and works forward
  from there. There is no answers file and nothing on the kernel command line
  to point at one. What DSKY is responsible for does work — the pinned ISO
  downloads, its hash matches, it is written unchanged, and it boots to that
  installer on UEFI — and that is the whole of what DSKY can promise here.
- Proxmox VE and TrueNAS SCALE: appliances, where desktop programs don't apply.
- Raspberry Pi OS: it has a first-boot customisation file, but it writes a
  finished system rather than running an installer, so it is a different
  feature.
- Your own installers (.msi/.exe) have no Linux equivalent here. A .deb or
  .rpm the operator adds could come later.
