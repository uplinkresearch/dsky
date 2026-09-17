<h1 align="center"><img src="docs/logo/dsky-cyan.png" alt="DSKY" width="480"></h1>

One tool to build bootable installation USB media for a fleet: pull and store
OS images, keep per-hardware driver packs, compose unattended install media
per machine/org (drivers + unattend + first-boot agents), and write verified
USB sticks. Runs on Windows, macOS, and Linux from a single static binary.

## Install

**Windows** — in PowerShell:

```powershell
irm https://raw.githubusercontent.com/uplinkresearch/dsky/main/install.ps1 | iex
```

Or download **dsky-setup-amd64.exe** from the
[latest release](https://github.com/uplinkresearch/dsky/releases/latest) and
double-click it.

**macOS and Linux** — in a terminal:

```sh
curl -fsSL https://raw.githubusercontent.com/uplinkresearch/dsky/main/install.sh | sh
```

Then start it:

```sh
dsky app
```

That opens the portal in its own window, where you can pick an operating
system and write a stick without setting anything up first.

No administrator or root rights are needed, and nothing touches system
directories. Each installer fetches the release build for your machine,
checks it against the release's SHA-256 list, and installs it under your own
profile — `%LOCALAPPDATA%\Programs\dsky` on Windows, where it is added to
your user PATH, or `~/.local/bin` on macOS and Linux, which is already on
PATH on most systems; if it is not, the installer prints the one line to add.
You also get a Start-menu, Launchpad or app-drawer entry. `dsky update` keeps
it current; `dsky uninstall` removes it.

Prefer to do it by hand, or on a machine that cannot reach GitHub? Every
platform's binary is on the
[releases page](https://github.com/uplinkresearch/dsky/releases/latest) —
one static file, no dependencies. Installing from a private fork? Set
`GITHUB_TOKEN` first. (If you download the macOS or Linux binary
with a browser rather than the command above, make it executable with
`chmod +x`, and on macOS clear the quarantine flag the browser set:
`xattr -d com.apple.quarantine dsky`.)

Org-agnostic by design: everything specific to an organization — recipes,
unattend templates, pinned download manifests, driver packs — lives in that
org's own **workspace** (a small git repo). `dsky init` scaffolds one in
seconds.

## About the name

DSKY — pronounced "DISS-kee" — was the astronauts' only interface to the Apollo
Guidance Computer: a numeric keypad and a display, driven by Verb/Noun number
pairs. Verb 37 Noun 01 was not a menu item. It was most of the vocabulary there
was, and it flew to the Moon.

What it shares with this tool is the situation rather than the styling. The
DSKY was the one panel between a person and a machine that would otherwise do
nothing useful, in a setting where a wrong entry was expensive and the display
had to say plainly what it was about to do.

<!-- TODO: dedication goes here. -->

## What it produces

- **Windows 10/11 unattended installers** — FAT32, UEFI-boot, `autounattend.xml`
  + `ei.cfg` + `$OEM$` payload; drivers install at first boot via `pnputil`
  (never DISM-injected), agents install at first boot (never baked — cloned
  agent identities collide in RMMs). WIMs over FAT32's 4 GiB limit are split
  automatically (DISM on Windows hosts, wimlib elsewhere).
- **Linux distro installers** — hybrid ISOs raw-written and verified; Ubuntu
  can be made **zero-touch**: `linux.autoinstall` renders cloud-init user-data
  into an appended CIDATA partition and rewrites the ISO's GRUB menu in place
  (same byte length, no ISO rebuild) to boot with `autoinstall`, so the
  installer never asks a question. Picking **programs** or **drivers** for an
  Ubuntu entry writes those answers with no workspace to author: apt packages
  and snaps go in the installer's own sections, Flathub apps and a vendor's
  own apt repository install at first boot, and Ubuntu's installer is told to
  put on the proprietary drivers it finds. Server installs unattended; Desktop
  keeps Ubuntu's own "Review your choices" confirmation before it erases
  anything.
- **Anaconda installers** — Fedora Server installed from a kickstart DSKY
  appends to the ISO, whose own bytes are left exactly as the distribution
  published them (`linux.kickstart`). Proven on Fedora Server 44: three minutes
  and no questions but the account. Picking **programs** writes that kickstart
  for you, with no workspace to author, from Fedora's own repositories, Flathub,
  a vendor's rpm repository or a published release — `dsky apps --os fedora`
  lists what Fedora gets and where each comes from. The answers are handed to
  the kernel as a second initramfs rather than a partition to mount: the ISO's
  volume label belongs to the whole disk on hybrid media, so the installer
  mounts the disk and no partition on it can then be opened. Fedora's shipped
  default entry also media-checks itself to a halt on any media with anything
  appended, so DSKY rewrites the boot menu in place, same byte length, as it
  does for Ubuntu.
- **Appliance images** — raw `.img` (xz/zstd/gz-compressed supported),
  stream-decompressed while writing.

UEFI-only by explicit constraint. Legacy BIOS boot is out of scope.

## How it works

**Compose to image, then flash.** DSKY builds a raw disk image file
(partition table + FAT32 + files) entirely in userspace — no admin rights, no
mounting — then the flash engine raw-writes it to the stick and verifies by
readback hash. One identical build path on all three OSes; images are
reproducible byte-for-byte (`SOURCE_DATE_EPOCH`), cacheable, and safe to
archive as masters.

Multi-gigabyte binaries never live in git. Workspace **manifests** pin
`url` + `sha256` (`dsky sources pull`); everything lands in a
machine-local content-addressed **library**. Official Windows ISOs need no
browser dance: a manifest with `provider: fido` resolves Microsoft's
rotating download links at pull time through the hash-pinned
[Fido](https://github.com/pbatard/Fido) helper, so
`dsky sources pull win11-iso` goes straight from nothing to the current
official Pro ISO. On a Mac or Linux this needs PowerShell 7, and DSKY runs a
copy of Fido with one line changed, because Fido refuses to run anywhere but
Windows. Without PowerShell, download the ISO from Microsoft's page (which
offers the file directly there) and hand it over with `--iso` or "Use an ISO
you downloaded".

**Bloat-free by recipe, not by modified media.** `windows.debloat` (presets
`standard`/`aggressive`, plus `remove_apps`/`keep_apps` overrides) generates
a first-boot pass that strips consumer apps (Xbox, Bing, Clipchamp, consumer
Teams, Solitaire, …), turns off Copilot, widgets, advertising ID, consumer
promotions and Start suggestions, and sets telemetry to the Pro floor — while
the installed media itself stays official, fully updatable, and
activation-safe.

**Driver assistance — find, not just stage.** `dsky drivers search dell
"OptiPlex 7010"` (or `lenovo`/`hp` by model) pulls the vendor's own
enterprise driver-pack catalog and lists the matching packs with versions,
dates, sizes, and hashes; `--add` writes a pinned, self-describing manifest
and downloads it. For hardware without a vendor feed — Intel NUCs, ASUS, a
lone unknown NIC — `dsky drivers search mscatalog "PCI\VEN_8086&DEV_15B8"`
queries the Microsoft Update Catalog by hardware ID and pulls the official
signed driver cab. A recipe names the machines it serves in a
`windows.hardware` block, and `compose` resolves and stages their packs
automatically (`dsky drivers resolve <recipe>` does it up front). The
manual tools remain: `drivers inspect` reads INFs (ANSI or UTF-16) out of a
directory/zip/cab and reports class, versions, and hardware IDs; `drivers
add` stages a pack you already have; `drivers scan` lists the local
machine's devices that still need drivers, with the IDs to search for.

Safety. The disk the running OS lives on is refused outright; that is not a
confirmation anyone can click through. Install media is written only to
removable USB. Copying a drive and the disk utility can also write to a fixed
disk, but only when a person picked that disk for that job, and it is
deliberately harder than writing to a stick. Before anything destructive, the
confirmation describes the target the way its owner would recognise it —
model, size, serial, partitions, labels, where it is mounted — and then asks
for its exact size to be typed. Stale GPT backup headers are wiped, and every
write is verified by readback (which also catches counterfeit flash). Reading
is never restricted: the system disk can be the source of a copy.

## Installing an operating system — pick one, no setup

Launch the app (Start-menu or app-launcher icon, or `dsky app`) and the home screen
has an **Install an OS** list, grouped into desktop, server, and single-board.
Pick one, choose a couple of options — for Windows: edition, **local account
vs. normal OOBE**, how much bloatware to strip, **drivers for this computer**,
**programs to install**, and an optional skip of the TPM/Secure-Boot/RAM
checks — plug in a stick, confirm its size, and it builds and flashes. No
workspace, no recipes. From the CLI:

```
dsky catalog
dsky install windows-11 --edition Pro --account local --debloat standard
```

Twenty-seven operating systems ship in the list today: Windows 11 and 10
(fetched from Microsoft on demand via Fido); Ubuntu 26.04 LTS desktop and
server plus 24.04 LTS server; Fedora 44 Workstation and Server; Debian 13;
Arch; Omarchy; CachyOS desktop and handheld; Linux Mint; Pop!_OS; Bazzite;
Nobara; Garuda; PikaOS; openSUSE Tumbleweed; NixOS 26.05; Red Hat Enterprise
Linux 10, AlmaLinux and Rocky Linux; Proxmox VE; TrueNAS SCALE; Raspberry Pi
OS; and Valve's Steam Deck recovery image.

Nothing is bundled. Each entry is a pinned pointer — URL plus SHA-256, or a
vendor checksum file for images whose URL always means "newest" — so the bytes
come from Microsoft, Canonical, Fedora or Valve directly and are verified on
arrival. They land in a machine-local library and are reused by later builds.

### When the download is refused

Microsoft rate-limits its ISO service to roughly one request per address per
day. Download the ISO yourself and hand it over once:

```
dsky install windows-11 --iso "C:\Users\you\Downloads\Win11.iso"
```

It is filed under that OS, so later builds skip the fetch. The wizard asks for
a path when it needs one, and the portal has a field for it.

### Drivers for the machine in front of you

`dsky detect` profiles this computer — make and model, CPU, GPUs, network
adapters, every PnP/PCI device — and says what it would fetch. Adding
`--drivers` to an install stages those drivers on the media, so the machine
comes up fully driven instead of hunting packs by hand:

```
dsky detect                 # what this computer is, and what it needs
dsky detect --resolve       # fetch those drivers now, cached for later
dsky install windows-11 --drivers
```

Machines from the makers listed below get their model's drivers -- DSKY
recognises the model from what the machine reports, such as an Intel NUC's
`NUC13ANKi7` for the NUC 13 Pro Kit; everything else resolves per device
through the Microsoft Update Catalog. (For
a machine you are sitting at, that is; the vendors below are picked by model.) Devices the catalogs
do not carry are reported and skipped rather than failing the build — Windows
Update covers most of them. GPU packages are large (easily a gigabyte each),
so driver media wants a 16 GB stick.

Building media for a machine you are *not* sitting at — the bench case, where
the target is a customer's fleet — names the model instead:

```
dsky install windows-11 --drivers-for "dell:OptiPlex 7010 Micro"
dsky drivers models dell        # every model Dell has a pack for
```

In the app, the Install dialog and the Payload screen have a searchable list of
every model these vendors publish Windows drivers for: Dell, HP, Lenovo,
Framework, Alienware, Microsoft Surface, ASUS, Intel NUC and Samsung Galaxy
Book. It is repeatable and combines with `--drivers`: pnputil installs only what
matches the hardware it finds, so one stick can carry packs for several models.

How each vendor's drivers go on differs, because each publishes them differently:

| Vendor | What a model gets | How it installs |
| --- | --- | --- |
| Dell, HP, Lenovo | the vendor's driver pack | unpacked, then pnputil |
| Intel NUC | Intel's INF driver pack, now hosted by ASUS | pnputil |
| Samsung | the Windows 11 DriverPack, or the driver zips for models without one | pnputil |
| Microsoft Surface | the model's driver MSI | unpacked with `msiexec /a`, then pnputil |
| Alienware | the newest release of each Dell Update Package for the model | unpacked with `/s /e=`, then pnputil |
| ASUS | the newest release of each driver installer for the model | ASUS's own silent switches, on that model only |
| Framework | Framework's driver bundle | run unattended, on that model only |

Dell, HP, Lenovo, Alienware and ASUS publish a hash for every download, and it
is checked. Microsoft and Samsung publish none, so each download is pinned by
its SHA-256 the first time it is fetched. Acer, MSI, LG and Razer are not
listed: Acer's and MSI's driver lists sit behind bot protection DSKY will not
pose as a browser to get past, LG publishes only network drivers per model,
and Razer's are firmware updaters with drivers left to Windows Update. Their
machines still get drivers per device through the Microsoft Update Catalog.

Framework is different in one way. It publishes a driver *bundle* per model
rather than a pack of drivers, and the bundle is Framework's own installer. It
runs unattended at first boot, and only on the model it is for: on any other
computer it is skipped, and it is stopped if it runs past 90 minutes. That path
has not yet been run on a real Framework.

### Programs

`dsky apps` lists the ninety-odd programs that can be installed alongside
the OS, from browsers to IT tools and game launchers; `--apps` picks them. They
install at first boot through winget, so nothing large rides on the media and
every installer comes from the vendor. A starter set (`set:business`,
`set:home`, `set:it`) adds a common group, and any other winget package works
by its id:

```
dsky apps
dsky install windows-11 --drivers --apps set:business,brave,winget:Mozilla.Firefox.ESR
```

The machine needs to be online at first boot for these — which is what the
staged network drivers are for. Some packages only install for the first
account that signs in; the list says which. Programs winget doesn't have, such
as RustDesk, go in with `dsky apps add` and ride on the stick.

### Programs on Ubuntu

The same picker, and the same `--apps`, for Ubuntu Server 24.04 and 26.04 and
Ubuntu Desktop 26.04 — the entries whose installer takes DSKY's answers:

```
dsky apps --os ubuntu            # what Ubuntu gets, and where each comes from
dsky install ubuntu-26.04-server --drivers --apps set:it,vlc,chrome
```

Fedora Server takes the same picker, through its kickstart, and its list is
not a translation of Ubuntu's: checked against Fedora's own metadata, `7zip`
rather than `p7zip`, Node.js by major version, and no Steam, HandBrake or
Telegram in Fedora's repositories at all — where Flathub has a
publisher-verified app it takes over, and where nothing does, the program is
not offered. RPM Fusion would supply the rest in one line and is deliberately
not enabled: it changes where the whole machine gets its updates, which is the
owner's decision rather than a side effect of ticking a box.

Each program comes from the best source Ubuntu has for it, in this order:
Ubuntu's own archive, then the Snap Store where the publisher is the vendor,
then Flathub for publisher-verified apps, then a vendor's own apt repository
where that is the only official channel — Google Chrome, AnyDesk and
TeamViewer — and last a published release binary, for a program packaged
nowhere at all. DSKY itself is the one of those: `--apps dsky` puts it in
`/usr/local/bin` on the machine being imaged, verified against the checksum
file published beside it, because the bench machine being imaged is usually
the machine that images the next one. `dsky apps --os ubuntu` names the source beside every program,
because what installs is often not spelled the way the program is. The picker
only offers what the chosen OS can install, so nothing Windows-only appears
for Ubuntu and nothing Ubuntu-only for Windows.

apt packages and snaps go into the installer's own `packages:` and `snaps:`
sections. Flathub apps and vendor repositories install at first boot from a
small systemd service, which also confirms that everything else arrived —
a machine whose network came up late finishes its install quietly missing
packages, and that pass is what notices and puts them on. Everything is logged
to `/var/log/dsky-apps.log`, one failure never stops the rest, and whatever
failed is tried again at the next boot.

`--drivers` on Ubuntu means what it says on the machine in front of you, but
by Ubuntu's route: nothing is staged, because the kernel already carries all of
it but the proprietary drivers, and the answers ask Ubuntu's installer to find
and install those (NVIDIA above all).

**What it erases, and what it asks.** Autoinstall takes the first disk without
asking, as DSKY's Windows sticks do. On Server that is the whole of it: the
install runs with no input except the account, which the installer asks for on
screen, because no password goes into DSKY's answers. On Desktop, Ubuntu's own
"Review your choices" screen is left in deliberately — it lists the answers and
waits for **Install**, so a stick booted on the wrong machine stops once before
erasing it.

### Programs on a machine that already runs Windows

Half of what a recipe does needs no operating system installed: the drivers for
the model, the programs the office uses, the consumer apps taken off. A recipe
can be built as a **payload** — one file that carries all of it to a machine
that already runs Windows:

```
dsky build front-desk --payload
```

or **Payload** on the portal's start screen, where the programs, the driver packs
for each computer model and the bloatware setting are chosen directly — or a
saved Windows recipe is built as one. Nothing is downloaded that would only
matter to installing an OS; no ISO is fetched at all.

Copy the `.exe` onto the machine and double-click it. It asks for an
administrator, unpacks itself into `C:\ProgramData\DSKY\payloads`, and shows
the same window a first boot shows, as an ordinary window. It installs
everything the recipe asks for, stops and asks before any restart Windows
needs, carries on after it, and removes the desktop shortcuts the installers
put there — but not the ones that were already on the desktop. It never erases
anything and never installs an operating system.

For a remote tool, with nobody at the machine:

```
<payload>.exe apply --quiet --unattended
```

The exit code is the number of problems; 0 is a clean run. The file is also a
zip, so anything that opens zips gives you the folder, the agent and a
README.txt — which is what to use where a program with a window cannot run.

### Joining a domain

Windows sticks can join a domain during Setup without a network, from files
made with `djoin /provision` on a computer already in the domain, so no domain
password goes on the stick. A join file is one computer's account:

- **One computer:** `--domain-blob PC-042.txt`, or "Join a domain" in the
  Install dialog. The PC gets the name in the file.
- **A batch from one stick:** `--domain-blobs <folder>`, a folder of join files
  each named after its computer's serial number (`5CG1234ABC.txt`). During
  Setup each PC joins with its own file and deletes all of them from its disk;
  a PC with no file stays out of the domain and logs why. The stick keeps every
  file, so wipe it when the batch is done.

### Three ways in, one pipeline

The same Quick Install path is driven by the CLI above, by `dsky tui`
(a full-screen terminal wizard — no flags to remember, works over SSH), and
by the web portal (`dsky serve`, or the Start-menu icon). A fix in the
pipeline shows up in all three.

### Twenty sticks at once

```
dsky flash media.img --all              # every stick attached
dsky clone <device> --to all            # read a master, write it to blanks
```

Targets are written in parallel under a **single** elevation — one prompt for
the batch, because twenty prompts would only teach people to script around
them. Each stick is independent, with its own handle and readback verify, so a
dead one fails alone and the error names it.

The interlock scales rather than repeating: one stick still asks for its exact
size; several list every target and ask you to type how many.

`dsky clone <device>` on its own reads a working stick into the library as a
master image — the generalized "capture the golden stick" workflow the original
NUC kit was built on. (`capture` still works as the old name.)

### Keeping it current

```
dsky update --check
dsky update
```

Replaces this binary with the newest published release, verified against the
checksums published beside it. The portal shows a banner when one is available.

This is the "distro-hop / image a machine in two clicks" path. For repeatable,
branded, fleet imaging with agents and per-model drivers, use a **workspace**:

## Quick start — "compose this"

```
dsky init --org "Acme IT" acme-workspace
cd acme-workspace
dsky example-win11
```

That last line is the whole job: it pulls any pinned source that is missing
(the official Windows ISO straight from Microsoft, driver cabs, agents),
composes the media, finds the one USB stick you have plugged in, asks you
to type its size, elevates once (UAC / polkit) for the raw write, writes,
and verifies every byte by readback. In a workspace with a single recipe,
plain `dsky` does the same. `dsky --build-only <recipe>` stops after
building; `dsky <recipe> <device>` names the stick when several are
attached.

The long form is still there when you want the pieces:

```
dsky sources pull win11-iso        # or: sources import win11-iso <path>
dsky build example-win11
dsky devices
dsky flash example-win11 <device-id>
```

The only step that ever needs elevated rights is the raw write to the USB
device itself, and that is one prompt per stick — installing and everything
else runs as a normal user.

`dsky serve` opens the same workflow as a local web page (loopback-only,
token-protected): recipes, devices, one-click builds, an arm-then-flash
dialog with the typed-size interlock enforced server-side, and live progress
over SSE. **Browse…** opens the host's own folder chooser to pick a workspace,
since a browser cannot hand a server an absolute path.

`dsky doctor` checks the host: on Windows and macOS the ISO/WIM tooling
is built into the OS (Mount-DiskImage/hdiutil, DISM); Linux needs `7zz` and
`wimlib`.

## Status

Early but real: the FAT32 image builder passes a native acid test (Windows mounts
a composed image, `chkdsk` reports zero problems, 400+ files hash-identical
through the Windows FAT driver, byte-reproducible builds), the full Windows
pipeline — captured-master trees, ISO extraction, overlays, driver packs,
generated first-boot scripts — is integration-tested, and the first physical
stick flashed on Windows verified 413/413 files through the OS FAT driver.
An Ubuntu stick has been flashed and verified on real hardware. Driver
auto-resolve is verified live against all four catalogs. The web UI, the
terminal wizard, and self-update are working.

**Ubuntu installs end to end, proven in virtual machines** (`test/autoinstall/vm.sh`,
which builds the media with DSKY from the real ISOs, installs from it onto a
blank disk, then boots the installed system and asks it what it got). Server
26.04 installs with no input in 8 minutes; Desktop 26.04 installs and reaches
the desktop. With programs picked the way the app picks them, all four sources
arrive on the installed machine: an apt package during setup, a snap, a Flathub
app, and Chrome from Google's own repository, with `/var/log/dsky-apps.log`
reporting no failures. `--drivers` writes an answer Ubuntu's installer accepts
and acts on. Every apt, snap and Flathub name in the program list is checked
against Ubuntu, the Snap Store and Flathub weekly.

Known gaps, stated plainly:

- **No physical Windows install has been done end to end yet** with drivers
  and programs staged. That is the next acceptance milestone, and until it
  passes, the first-boot driver and winget paths are reviewed but unproven.
  Domain join has not yet joined a real domain.
- **Copying a drive has never touched a real disk.** The clone engine's
  logic is tested — fanning out to many targets, a stick pulled mid-write, a
  drive that stores something other than what it was sent, every mistaken
  pairing — but against an in-memory target. The platform write path it
  depends on is not exercised by any of that.
- **The race detector has never run** over the test suite.
- **No physical Ubuntu install with programs has been done yet.** The whole
  path is proven in virtual machines, on the real ISOs, but a VM is not a
  computer: it has no proprietary drivers for `--drivers` to find, and its
  network is the host's. Real hardware is the remaining acceptance step.
- **Ubuntu's programs need the machine online.** apt packages and snaps are
  installed by Ubuntu's own installer, and Flathub apps and vendor
  repositories at first boot; an offline machine gets none of them. It is not
  silent about it — whatever failed is logged and tried again at the next boot
  — but there is no equivalent of the Windows path's installers riding on the
  stick.
- **Parallel multi-stick writing has not been run on more than one stick.**
  The engine is there and its guards are tested; the concurrency is not
  hardware-proven.
- **macOS and Linux flash paths** are written but not hardware-tested.
- **Windows Server** is not in the catalog. Fido cannot fetch it and its
  edition selection needs WIM image names read from a real ISO rather than
  guessed.
- **Binaries are unsigned**, so SmartScreen and Smart App Control will object,
  and self-update verifies integrity rather than authorship. Code signing is
  the fix for both.

## Development

```
go build ./cmd/dsky
go test ./... -short
```

Pure Go plus `github.com/diskfs/go-diskfs` for partition tables and FAT32
(with two workarounds found by the acid test: io/fs-style paths for reads,
and a post-pass fixing `..` cluster entries of top-level directories to 0
per the FAT spec). External tools (7-Zip, wimlib, DISM) are always invoked
as subprocesses, never linked.

MIT licensed.
