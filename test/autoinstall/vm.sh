#!/usr/bin/env bash
# Boot a real Ubuntu ISO, built into install media by DSKY's linux.autoinstall,
# in a virtual machine: install onto a blank disk with nobody at the keyboard,
# then boot the installed system and ask it what it got.
#
# DSKY's autoinstall path was only ever checked against a synthetic ISO. This
# is the proof on the ISOs people actually download. It never touches a real
# disk: the "stick" is the built image file and the target is a raw disk file.
#
#   test/autoinstall/vm.sh <case> [workdir]
#
# Cases:
#   server-24.04          Ubuntu Server 24.04, zero-touch (GRUB patched)
#   server-26.04          Ubuntu Server 26.04, zero-touch
#   desktop-26.04         Ubuntu Desktop 26.04, zero-touch, account in the
#                         answers: does the mechanism work on the desktop ISO?
#   desktop-26.04-prompt  Ubuntu Desktop 26.04 as Quick Install would make it:
#                         GRUB left alone (Ubuntu's own prompt kept) and no
#                         account in the answers. Observed, not pass/fail:
#                         screenshots show what a person would see, and Install
#                         is pressed at the review screen to see what follows.
#   server-26.04-programs Ubuntu Server 26.04 with programs picked the way the
#                         app picks them (dsky install --apps): one of each
#                         kind of source — an Ubuntu package, a snap, a Flathub
#                         app, Chrome from Google's repository and DSKY from
#                         its own published release — checked on the installed
#                         system after first boot.
#   desktop-26.04-programs  The same programs through the Desktop installer,
#                         which keeps Ubuntu's confirmation screen: the run
#                         waits for the review screen and presses Install, as
#                         a person would, then checks what landed.
#   fedora-44-kickstart   Fedora Server 44 installed unattended from a kickstart
#                         DSKY appends to the ISO, which is otherwise left
#                         exactly as Fedora published it. The answers ride in as
#                         a second initramfs rather than a partition to mount —
#                         see internal/compose/cpio.go for why the obvious way
#                         cannot work on media made from a hybrid ISO.
#   omarchy               Omarchy written as its own ISO, unchanged, and booted
#                         on UEFI. It has no unattended path — archiso goes
#                         straight into Omarchy's own installer — so this
#                         checks the part DSKY is responsible for and records
#                         what a person sees. Observed, not pass/fail.
#   server-26.04-drivers  `dsky install --drivers` on Ubuntu, which asks its
#                         installer for the proprietary drivers it finds. A VM
#                         has none; what this proves is that the section DSKY
#                         writes is one the installer accepts and acts on.
#
# The installed system is checked by logging into it over SSH and running the
# checks there, so nothing here needs root on the host: no loop devices, no
# qemu-nbd, no mounting. The answers each case writes carry a test-only SSH
# key for that; a real DSKY build has none. Raw disk files are used rather than
# qcow2 so qemu-img is not needed either.
#
# Needs: go, qemu-system-x86_64 with KVM, OVMF, socat, ssh, ImageMagick
# (convert). Screenshots land in <workdir>/shots as PNG; results in
# <workdir>/result.md.
#
# Handy environment:
#   DSKY_ISO_DIR   a directory of already-downloaded ISOs; one matching the
#                  case's file name is imported instead of downloaded again.
#   UBUNTU_MIRROR  where to download from otherwise.
#   OVMF_CODE / OVMF_VARS   firmware paths, if this distribution hides them
#                  somewhere not on the list below.
#   KEEP_ISO=1     do not delete the pulled ISO after the build.
set -euo pipefail

CASE=${1:?case}
W=$(realpath -m "${2:-./autoinstall-work}")
mkdir -p "$W/shots"

# Run from a copy in the workdir. A case takes the better part of an hour, and
# bash reads a script as it goes rather than all at once: editing this file
# while a run is in flight changes what the run does halfway through. That has
# happened twice, and both times the result looked like a finding rather than
# like the harness being edited underneath itself. REPO is worked out here and
# carried over, since the copy cannot find the repository from where it sits.
if [ -z "${DSKY_VM_PINNED:-}" ]; then
  DSKY_VM_REPO=$(cd "$(dirname "$0")/../.." && pwd)
  cp "$0" "$W/vm.sh"
  export DSKY_VM_PINNED=1 DSKY_VM_REPO
  exec bash "$W/vm.sh" "$@"
fi
REPO=$DSKY_VM_REPO
RESULT="$W/result.md"
: >"$RESULT"
say() { echo "$*" | tee -a "$RESULT"; }

# releases.ubuntu.com sent GitHub's runners 0.2-1.4 MiB/s, and every case
# but one ran out of time still downloading. The kernel.org mirror carries the
# same files; the pinned sha256 below is what makes any mirror safe to use.
MIRROR=${UBUNTU_MIRROR:-https://mirrors.edge.kernel.org/ubuntu-releases}

case "$CASE" in
server-24.04)
  URL=$MIRROR/24.04/ubuntu-24.04.5-live-server-amd64.iso
  SHA=97f3d7ffb032c3eb3b23d2c8be9cc76e60c2c1f2c0146ba5ba9fe01cafae0fd8
  KIND=server PATCH=true IDENTITY=true OBSERVE=false ;;
server-26.04)
  URL=$MIRROR/26.04/ubuntu-26.04.1-live-server-amd64.iso
  SHA=cc8a95cde20f6ced61a322420de00f10cc3c90ced545daa46cb9c1a117f1d927
  KIND=server PATCH=true IDENTITY=true OBSERVE=false ;;
desktop-26.04)
  URL=$MIRROR/26.04/ubuntu-26.04.1-desktop-amd64.iso
  SHA=601e30fbf5d97759367c632e2c33630665039b7e2158fd068403da3ccf1bda1f
  KIND=desktop PATCH=true IDENTITY=true OBSERVE=false ;;
desktop-26.04-prompt)
  URL=$MIRROR/26.04/ubuntu-26.04.1-desktop-amd64.iso
  SHA=601e30fbf5d97759367c632e2c33630665039b7e2158fd068403da3ccf1bda1f
  KIND=desktop PATCH=false IDENTITY=false OBSERVE=true ;;
server-26.04-programs)
  URL=$MIRROR/26.04/ubuntu-26.04.1-live-server-amd64.iso
  SHA=cc8a95cde20f6ced61a322420de00f10cc3c90ced545daa46cb9c1a117f1d927
  KIND=server PATCH=true IDENTITY=true OBSERVE=false
  PROGRAMS=vlc,brave,obsidian,chrome,dsky SRC_ID=ubuntu-26.04-server ;;
desktop-26.04-programs)
  URL=$MIRROR/26.04/ubuntu-26.04.1-desktop-amd64.iso
  SHA=601e30fbf5d97759367c632e2c33630665039b7e2158fd068403da3ccf1bda1f
  KIND=desktop PATCH=false IDENTITY=true OBSERVE=false
  PROGRAMS=vlc,brave,obsidian,chrome,dsky SRC_ID=ubuntu-26.04-desktop
  # Desktop keeps Ubuntu's confirmation: Enter at the review screen, as a
  # person would press Install.
  INSTALL_BUTTON=true ;;
fedora-44-kickstart)
  # The basis of DSKY's kickstart path, which had never been run against a real
  # Anaconda ISO. Several things had to be right before it worked at all; each
  # is written down where it lives.
  URL=https://dl.fedoraproject.org/pub/fedora/linux/releases/44/Server/x86_64/iso/Fedora-Server-dvd-x86_64-44-1.7.iso
  SHA=85837793bfa36db6bc709b4cecd2ec116951b87d9c53c3d95eb2fac8dcf7cf1f
  KIND=server FAMILY=fedora PATCH=false IDENTITY=true OBSERVE=false
  SRC_ID=fedora-44-server ;;
omarchy)
  # Omarchy has no unattended path: archiso boots straight into Omarchy's own
  # installer (timeout=0, menu hidden) and asks its questions there, which is
  # why it is not in the program picker. What can be checked is the part DSKY
  # is responsible for — that the pinned ISO is written as the distribution
  # made it, and boots to that installer on UEFI.
  URL=https://iso.omarchy.org/omarchy-4.0.3.iso
  SHA=03d60bc74306dca51f96e1a84b690871d8d606826b260edd0208962da8507d14
  KIND=desktop FAMILY=plain PATCH=false IDENTITY=false OBSERVE=true
  SRC_ID=omarchy ;;
server-26.04-drivers)
  # --drivers on Ubuntu asks its installer to put on the proprietary drivers
  # it finds. A virtual machine has none, so what this proves is the part
  # that can go wrong everywhere: that the section DSKY writes is one the
  # installer accepts, and that it ran rather than refusing the answers.
  URL=$MIRROR/26.04/ubuntu-26.04.1-live-server-amd64.iso
  SHA=cc8a95cde20f6ced61a322420de00f10cc3c90ced545daa46cb9c1a117f1d927
  KIND=server PATCH=true IDENTITY=true OBSERVE=false
  PROGRAMS=vlc SRC_ID=ubuntu-26.04-server DRIVERS=true ;;
*) echo "unknown case $CASE" >&2; exit 2 ;;
esac
PROGRAMS=${PROGRAMS:-}
DRIVERS=${DRIVERS:-}
# Which installer answers the media carries: Ubuntu's cloud-init autoinstall on
# a CIDATA partition, or Anaconda's kickstart as a second initramfs.
FAMILY=${FAMILY:-ubuntu}
SRC_ID=${SRC_ID:-ci-ubuntu-iso}
# Ubuntu Desktop stops at "Ready to install — Review your choices" and waits,
# which is the one confirmation before anything is erased. The cases that get
# past it press Install the way a person does.
INSTALL_BUTTON=${INSTALL_BUTTON:-}
[ "$CASE" = desktop-26.04-prompt ] && INSTALL_BUTTON=true

say "## $CASE"
say ""
say "- ISO: \`$(basename "$URL")\`"
if [ "$FAMILY" = plain ]; then
  say "- The distribution's own ISO, written unchanged: nothing appended, nothing rewritten"
elif [ "$FAMILY" = fedora ]; then
  say "- Anaconda kickstart appended to the ISO; the ISO's own bytes are untouched"
else
  say "- GRUB patched for zero-touch: $PATCH · account in answers: $IDENTITY"
fi

# ── A test-only key, so the installed system can be asked what it got ──────
KEY="$W/id_ed25519"
[ -f "$KEY" ] || ssh-keygen -q -t ed25519 -N "" -C dsky-vm-test -f "$KEY"
PUBKEY=$(cat "$KEY.pub")
SSH_PORT=${SSH_PORT:-$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')}
ssh_run() {
  ssh -q -i "$KEY" -p "$SSH_PORT" \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o ConnectTimeout=10 -o LogLevel=ERROR \
    root@127.0.0.1 "$@"
}

# ── Build the media with DSKY, as a user would ─────────────────────────────
export DSKY_LIBRARY="$W/lib"
(cd "$REPO" && CGO_ENABLED=0 go build -o "$W/dsky" ./cmd/dsky)
rm -rf "$W/ws"
"$W/dsky" init --org "DSKY CI" "$W/ws" >/dev/null

cat >"$W/ws/manifests/$SRC_ID.yaml" <<EOF
id: $SRC_ID
kind: os-image
format: iso
url: $URL
sha256: $SHA
EOF

# The SSH scaffolding every case adds to its answers. openssh-server comes
# from the installer's own ssh section; the key goes to root, whose sshd
# default (prohibit-password) accepts a key and no password, so the checks
# need no password anywhere.
ssh_answers() { # indent
  echo "  ssh:"
  echo "    install-server: true"
  echo "    allow-pw: false"
  echo "    authorized-keys:"
  echo "      - \"$PUBKEY\""
}
ssh_late_commands() { # the root key, for the cases whose ssh section may not run
  echo "    - mkdir -p /target/root/.ssh"
  echo "    - chmod 0700 /target/root/.ssh"
  echo "    - echo '$PUBKEY' > /target/root/.ssh/authorized_keys"
  echo "    - chmod 0600 /target/root/.ssh/authorized_keys"
  echo "    - curtin in-target --target=/target -- systemctl enable ssh"
}

if [ "$FAMILY" = plain ]; then
  # Nothing appended and nothing rewritten: the distribution's own ISO, which
  # is what DSKY writes for an entry whose installer takes no answers.
  cat >"$W/ws/recipes/ci-ubuntu.yaml" <<EOF
version: 1
id: ci-ubuntu
name: "CI: $CASE"
os:
  type: linux-iso
  source: $SRC_ID
target:
  min_stick: 8GiB
  boot: uefi-only
flash:
  verify: readback-sha256
EOF
elif [ "$FAMILY" = fedora ]; then
  # A kickstart that answers everything Anaconda would otherwise ask, so that
  # anything left on screen is a failure rather than a question. The account
  # and key are the test's, the way the Ubuntu cases' are.
  cat >"$W/ws/templates/ci-kickstart.cfg.tmpl" <<KSEOF
# Generated for DSKY's CI ({{.Org.Name}}).
text
lang en_US.UTF-8
keyboard us
timezone UTC --utc
network --bootproto=dhcp --activate --hostname=dsky-ci
rootpw --lock
user --name=dsky --groups=wheel --password=dsky --plaintext
clearpart --all --initlabel
autopart --type=plain --nohome
bootloader --location=mbr
firstboot --disable
services --enabled=sshd
%packages
@^server-product-environment
# On the DVD (checked in its Packages tree) and not in the default selection,
# so finding it afterwards means %packages was honoured. A package that is not
# on the media fails the whole install with "No match for argument", which is
# how this line was chosen rather than guessed.
vim-enhanced
%end
%post
mkdir -p /root/.ssh
chmod 700 /root/.ssh
echo '$PUBKEY' > /root/.ssh/authorized_keys
chmod 600 /root/.ssh/authorized_keys
echo "provisioned by DSKY" > /etc/dsky-provisioned
%end
reboot
KSEOF
  cat >"$W/ws/recipes/ci-ubuntu.yaml" <<EOF
version: 1
id: ci-ubuntu
name: "CI: $CASE"
os:
  type: linux-iso
  source: $SRC_ID
target:
  min_stick: 8GiB
  boot: uefi-only
linux:
  kickstart:
    file: templates/ci-kickstart.cfg.tmpl
    # The installer's console on the serial port, which the run captures to
    # serial-install.log. A headless server install has nowhere else to say
    # why it stopped, and screenshots of a scrolling dracut log are a poor
    # substitute for the log.
    kernel_args: [console=ttyS0,115200, inst.text]
flash:
  verify: readback-sha256
EOF
fi

{
  echo '#cloud-config'
  echo 'autoinstall:'
  echo '  version: 1'
  if [ "$IDENTITY" = true ]; then
    # Test-only account: user dsky, password "dsky".
    echo '  identity:'
    echo '    hostname: dsky-ci'
    echo '    username: dsky'
    echo "    password: \"$(openssl passwd -6 -salt dskyci dsky)\""
  fi
  ssh_answers
  echo '  storage:'
  echo '    layout:'
  echo '      name: direct'
  echo '  packages:'
  echo '    - hello'
  echo '  snaps:'
  echo '    - name: hello-world'
  echo '  late-commands:'
  echo '    - echo "provisioned by DSKY ({{.Org.Name}})" > /target/etc/dsky-provisioned'
  ssh_late_commands
  echo '  shutdown: reboot'
} >"$W/ws/templates/ci-autoinstall.yaml.tmpl"

# An ISO already on this machine is imported rather than downloaded again.
# This happens before anything is built, so the programs case (whose source id
# is the catalog's own) finds it in the library instead of fetching it.
LOCAL_ISO="${DSKY_ISO_DIR:-}/$(basename "$URL")"
if [ "${DRYRUN:-}" != 1 ] && [ -n "${DSKY_ISO_DIR:-}" ] && [ -f "$LOCAL_ISO" ]; then
  say "- ISO taken from \`$DSKY_ISO_DIR\` rather than downloaded"
  "$W/dsky" -w "$W/ws" sources import "$SRC_ID" "$LOCAL_ISO"
fi

if [ -n "$PROGRAMS" ]; then
  # Build exactly what the app builds for these programs, which also downloads
  # Ubuntu the way DSKY does (fastest mirror), then take its answers and add
  # the test account and SSH key: the real ones ask for the account on screen.
  driverflag=""
  [ "$DRIVERS" = true ] && driverflag="--drivers"
  "$W/dsky" install "$SRC_ID" --apps "$PROGRAMS" $driverflag --build-only
  gen="$W/lib/quick/templates/dsky-ubuntu-$SRC_ID.yaml.tmpl"
  [ -s "$gen" ] || { say "- **FAIL** dsky install --apps wrote no answers"; exit 1; }
  cp "$gen" "$W/generated-user-data.yaml"
  python3 - "$gen" "$W/ws/templates/ci-autoinstall.yaml.tmpl" \
    "$(openssl passwd -6 -salt dskyci dsky)" "$PUBKEY" <<'PYEOF'
import sys
src, dst, pw, pubkey = sys.argv[1:]
text = open(src).read()

ident = ('  identity:\n    hostname: dsky-ci\n'
         '    username: dsky\n    password: "%s"\n' % pw)
scaffold = ident + ('  ssh:\n    install-server: true\n    allow-pw: false\n'
                    '    authorized-keys:\n      - "%s"\n' % pubkey)

# Server's answers leave the account to the on-screen question; Desktop's
# leave it to Ubuntu's own installer. Either way the test needs an account
# it knows, so the scaffolding replaces the one or is added after version:.
interactive = "  interactive-sections:\n    - identity\n"
if interactive in text:
    text = text.replace(interactive, scaffold, 1)
else:
    text = text.replace("  version: 1\n", "  version: 1\n" + scaffold, 1)

# The root key, so the checks can read anything without a password. These go
# with whatever late-commands DSKY generated, or start the section.
keylines = ("    - mkdir -p /target/root/.ssh\n"
            "    - chmod 0700 /target/root/.ssh\n"
            "    - echo '%s' > /target/root/.ssh/authorized_keys\n"
            "    - chmod 0600 /target/root/.ssh/authorized_keys\n"
            "    - curtin in-target --target=/target -- systemctl enable ssh\n" % pubkey)
if "  late-commands:\n" in text:
    text = text.replace("  late-commands:\n", "  late-commands:\n" + keylines, 1)
else:
    text = text.replace("  shutdown: reboot", "  late-commands:\n" + keylines + "  shutdown: reboot", 1)
open(dst, "w").write(text)
PYEOF
  say "- \`dsky install $SRC_ID --apps $PROGRAMS $driverflag\` built its media; its answers are reused with a test account and SSH key"
  find "$W/lib/artifacts" -type f -size +1G -delete || true
fi

if [ "$FAMILY" = ubuntu ]; then
cat >"$W/ws/recipes/ci-ubuntu.yaml" <<EOF
version: 1
id: ci-ubuntu
name: "CI: $CASE"
os:
  type: linux-iso
  source: $SRC_ID
target:
  min_stick: 8GiB
  boot: uefi-only
linux:
  autoinstall:
    user_data: templates/ci-autoinstall.yaml.tmpl
    kernel_patch: $PATCH
flash:
  verify: readback-sha256
EOF
fi

# DRYRUN=1 stops here, after checking the workspace, without the multi-GB pull.
if [ "${DRYRUN:-}" = 1 ]; then
  "$W/dsky" -w "$W/ws" recipes list
  "$W/dsky" -w "$W/ws" sources list
  case "$FAMILY" in
  plain)  echo "(no answers: the ISO is written as the distribution made it)" ;;
  fedora) cat "$W/ws/templates/ci-kickstart.cfg.tmpl" ;;
  *)      cat "$W/ws/templates/ci-autoinstall.yaml.tmpl" ;;
  esac
  exit 0
fi

"$W/dsky" -w "$W/ws" sources pull "$SRC_ID"
"$W/dsky" -w "$W/ws" build ci-ubuntu | tee "$W/build.log"
IMG=$(awk '/^artifact:/ {print $2}' "$W/build.log")
[ -f "$IMG" ] || { say "- **FAIL** build produced no image"; exit 1; }
say "- DSKY built \`$(basename "$IMG")\` ($(( $(stat -c %s "$IMG") >> 20 )) MiB)"
# The pulled ISO is no longer needed; runners are short of disk.
[ "${KEEP_ISO:-}" = 1 ] || find "$W/lib" -type f -size +1G ! -samefile "$IMG" -delete || true

# ── VM plumbing ─────────────────────────────────────────────────────────────
find_firmware() { # name of the variable, then candidates
  local -n out=$1; shift
  [ -n "${out:-}" ] && [ -f "${out:-}" ] && return 0
  local c
  for c in "$@"; do [ -f "$c" ] && { out=$c; return 0; }; done
  echo "vm.sh: no OVMF firmware found (tried: $*); set OVMF_CODE and OVMF_VARS" >&2
  return 1
}
OVMF_CODE=${OVMF_CODE:-}
OVMF_VARS=${OVMF_VARS:-}
find_firmware OVMF_CODE /usr/share/OVMF/OVMF_CODE_4M.fd /usr/share/edk2/x64/OVMF_CODE.4m.fd \
  /usr/share/edk2-ovmf/x64/OVMF_CODE.fd /usr/share/qemu/ovmf-x86_64-code.bin
find_firmware OVMF_VARS /usr/share/OVMF/OVMF_VARS_4M.fd /usr/share/edk2/x64/OVMF_VARS.4m.fd \
  /usr/share/edk2-ovmf/x64/OVMF_VARS.fd /usr/share/qemu/ovmf-x86_64-vars.bin
cp "$OVMF_VARS" "$W/vars.fd"
# A raw sparse file rather than qcow2, so qemu-img is not needed.
rm -f "$W/target.raw"
truncate -s 40G "$W/target.raw"

QPID=""; MON=""
start_vm() { # phase, extra qemu args...
  local phase=$1; shift
  MON="$W/mon-$phase.sock"
  rm -f "$MON"
  qemu-system-x86_64 -enable-kvm -machine q35 -cpu host -smp 4 -m 8G \
    -drive if=pflash,format=raw,readonly=on,file="$OVMF_CODE" \
    -drive if=pflash,format=raw,file="$W/vars.fd" \
    -drive file="$W/target.raw",if=virtio,format=raw \
    -netdev user,id=net0,hostfwd=tcp:127.0.0.1:"$SSH_PORT"-:22 \
    -device virtio-net-pci,netdev=net0 \
    -device qemu-xhci -vga std -display none \
    -serial file:"$W/serial-$phase.log" \
    -monitor unix:"$MON",server,nowait "$@" &
  QPID=$!
}
mon() { echo "$*" | socat - UNIX-CONNECT:"$MON" >/dev/null 2>&1 || true; }
shot() { mon "screendump $W/shots/$1-$(printf %03d "$2").ppm"; }

# press_install presses Ubuntu Desktop's Install button, if its review screen
# is what is on the screen right now.
#
# Two things had to be learned the hard way here. The screen is watched for the
# button rather than the keys being sent after a fixed number of minutes,
# because how long the review screen takes to appear depends on the machine.
# And Enter on its own does nothing: the dialog puts the focus in the
# scrollable list of answers, which takes the key itself, so Tab has to move it
# to the button first. A run that pressed only Enter sat at the review screen
# until it timed out, with screenshots that looked like the installer had hung.
press_install() { # screendump
  python3 "$REPO/test/autoinstall/findbutton.py" "$1" >/dev/null 2>&1 || return 1
  mon "sendkey tab"
  sleep 1
  mon "sendkey ret"
  return 0
}
vm_running() { [ -n "$QPID" ] && kill -0 "$QPID" 2>/dev/null; }
stop_vm() {
  vm_running || return 0
  mon quit
  local i
  for i in $(seq 1 20); do vm_running || return 0; sleep 1; done
  kill "$QPID" 2>/dev/null || true
  wait "$QPID" 2>/dev/null || true
}

run_vm() { # phase, seconds-limit, extra qemu args...  (waits for the VM to end)
  local phase=$1 limit=$2; shift 2
  start_vm "$phase" "$@"
  local start=$SECONDS n=0
  local pressed=""
  while vm_running; do
    sleep 30
    n=$((n + 1))
    shot "$phase" "$n"
    # Tried again every time the review screen is still the screen: one press
    # that did not take would otherwise sit there until the run timed out,
    # and pressing again once it has gone past costs nothing.
    if [ "$phase" = install ] && [ "$INSTALL_BUTTON" = true ]; then
      if press_install "$W/shots/$phase-$(printf %03d "$n").ppm"; then
        if [ -z "$pressed" ]; then
          pressed=1
          say "- Ubuntu's review screen appeared after $(( (SECONDS - start) / 60 )) min; pressed Install"
        fi
      fi
    fi
    if [ $((SECONDS - start)) -ge "$limit" ]; then
      stop_vm
      return 124
    fi
  done
  wait "$QPID" 2>/dev/null || true
  return 0
}

shots_to_png() {
  for f in "$W"/shots/*.ppm; do
    [ -e "$f" ] || continue
    convert "$f" "${f%.ppm}.png" && rm -f "$f"
  done
}
trap 'stop_vm; shots_to_png' EXIT

# ── Install from the stick ─────────────────────────────────────────────────
# The image is attached as a USB stick and boots first. -no-reboot turns the
# installer's final reboot into QEMU exiting, which is how the end is seen.
#
# snapshot=on, not readonly=on: a real USB stick is writable, and an installer
# is entitled to write to one. Anaconda mounts the answers partition read-write
# -- it treats an OEMDRV volume as a driver disk as well as a kickstart -- and
# against read-only media that mount fails with "Can't open blockdev", which
# looks like a broken partition and is not. Writes go to a throwaway overlay,
# so the built artifact is still never modified.
LIMIT=$((60 * 60))
[ "$OBSERVE" = true ] && LIMIT=$((30 * 60))
set +e
t0=$SECONDS
run_vm install "$LIMIT" -no-reboot \
  -drive file="$IMG",format=raw,if=none,id=stick,snapshot=on \
  -device usb-storage,drive=stick,bootindex=0
rc=$?
set -e
mins=$(( (SECONDS - t0) / 60 ))

if [ "$OBSERVE" = true ]; then
  say "- Observed for $mins min (exit $rc); see the install-* screenshots for what a person sees."
  exit 0
fi
if [ $rc -eq 124 ]; then
  say "- **FAIL** install still running after $mins min (last screenshots show where it stopped)"
  exit 1
fi
if [ $mins -lt 3 ]; then
  say "- **FAIL** the VM stopped after $mins min — too soon to have installed anything"
  exit 1
fi
say "- Installer finished and rebooted after $mins min"

# ── First boot of the installed system, and what it got ────────────────────
fail=0
check() { # description, command run inside the installed system
  local what=$1; shift
  if ssh_run "$@" >/dev/null 2>&1; then say "- ok: $what"; else say "- **FAIL**: $what"; fail=1; fi
}
# Some of what the answers ask for lands after the login prompt does: snapd
# installs the snaps from the snaps: section as an ordinary store install once
# the system is up, so a check made the moment SSH answers is checking too
# early. These retry until the work is done or the wait runs out.
check_eventually() { # seconds, description, command
  local limit=$1 what=$2; shift 2
  local t0=$SECONDS
  while [ $((SECONDS - t0)) -lt "$limit" ]; do
    if ssh_run "$@" >/dev/null 2>&1; then
      say "- ok: $what$( [ $((SECONDS - t0)) -gt 20 ] && echo " (after $((SECONDS - t0))s)")"
      return 0
    fi
    sleep 15
  done
  say "- **FAIL**: $what (still not there after $limit s)"
  ssh_run "snap changes" >>"$W/snap-changes.txt" 2>&1 || true
  fail=1
  return 1
}
# snapd is done when nothing is in flight: a change still "Doing" is a snap
# still downloading.
wait_for_snapd() { # seconds
  local limit=$1 t0=$SECONDS
  while [ $((SECONDS - t0)) -lt "$limit" ]; do
    ssh_run "! snap changes 2>/dev/null | grep -qE '^[0-9]+ +(Doing|Do) '" >/dev/null 2>&1 && return 0
    sleep 15
  done
  return 1
}

start_vm firstboot
# Wait for the installed system to come up and answer.
BOOT_WAIT=$((8 * 60))
up=false
t0=$SECONDS n=0
while vm_running && [ $((SECONDS - t0)) -lt $BOOT_WAIT ]; do
  if ssh_run true >/dev/null 2>&1; then up=true; break; fi
  sleep 15
  n=$((n + 1)); shot firstboot "$n"
done
if [ "$up" != true ]; then
  say "- **FAIL** the installed system did not answer over SSH within $((BOOT_WAIT / 60)) min"
  say "  (the installer may have finished without installing openssh-server; see serial-firstboot.log)"
  exit 1
fi
say "- The installed system booted and answered over SSH after $(( (SECONDS - t0) / 60 )) min $(( (SECONDS - t0) % 60 ))s"

if [ -n "$PROGRAMS" ]; then
  # The first-boot service installs Flathub apps and Chrome, and snapd seeds
  # the snaps. Wait for DSKY's own done marker rather than a fixed sleep.
  APPS_WAIT=$((30 * 60))
  t0=$SECONDS
  while [ $((SECONDS - t0)) -lt $APPS_WAIT ]; do
    ssh_run test -e /var/lib/dsky/apps-done >/dev/null 2>&1 && break
    sleep 20
    n=$((n + 1)); shot firstboot "$n"
  done
  ssh_run cat /var/log/dsky-apps.log >"$W/dsky-apps.log" 2>/dev/null || true
  say "- First-boot programs took $(( (SECONDS - t0) / 60 )) min"

  # Each program is checked where it was meant to come from, and only when
  # this case picked it: the cases do not all pick the same list.
  picked() { case ",$PROGRAMS," in *",$1,"*) return 0 ;; esac; return 1; }
  if picked vlc; then
    check "Ubuntu package installed during setup (vlc)" \
      "dpkg-query -W -f='\${Status}' vlc | grep -q 'install ok installed'"
  fi
  if picked brave; then
    wait_for_snapd $((15 * 60)) || true
    check_eventually $((10 * 60)) "snap installed (brave)" "snap list brave" || true
  fi
  if picked obsidian; then
    check "Flathub app installed (md.obsidian.Obsidian)" "flatpak info md.obsidian.Obsidian"
  fi
  if picked chrome; then
    check "Chrome installed from Google's repository" \
      "dpkg-query -W -f='\${Status}' google-chrome-stable | grep -q 'install ok installed'"
  fi
  if picked dsky; then
    # On every user's PATH, not root's, and the binary the checksum passed is
    # the one that is there: it has to run.
    check "DSKY installed from its own release, and runs" \
      "/usr/local/bin/dsky version | grep -q '^dsky v'"
    check "DSKY is on the PATH for an ordinary account" \
      "su - dsky -c 'command -v dsky' | grep -q '^/usr/local/bin/dsky$'"
    check "DSKY has an app-drawer launcher every account can see" \
      "grep -q '^Exec=/usr/local/bin/dsky app$' /usr/local/share/applications/dsky.desktop"
  fi
  check "first-boot programs finished (/var/lib/dsky/apps-done)" \
    "test -e /var/lib/dsky/apps-done"
  check "the first-boot pass ran and logged no failure" \
    "test -s /var/log/dsky-apps.log && ! grep -q FAILED /var/log/dsky-apps.log"
  if [ "$DRIVERS" = true ]; then
    check "the answers asked Ubuntu for third-party drivers" \
      "grep -A2 '^  drivers:' /var/log/installer/autoinstall-user-data | grep -q 'install: true'"
    # What a virtual machine can prove is that the install step ran, not that
    # anything was installed: there is no proprietary hardware here to install
    # for. Most of what looks like proof is not. Every Ubuntu install loads a
    # drivers controller and runs `ubuntu-drivers list` to see what is on the
    # machine, so "drivers", "ubuntu-drivers" and the controller's own module
    # name are all in the log of an install that was never asked for any —
    # checked against this suite's other cases, which have 27 lines matching
    # "drivers" and none matching this. The install step is what only happens
    # when the answers ask for it.
    check "the installer's drivers step ran" \
      "grep -q 'drivers-install: installing third-party drivers' /var/log/installer/subiquity-server-debug.log"
  fi
elif [ "$FAMILY" = fedora ]; then
  # The claim: the installer ran start to finish on answers DSKY put on the
  # stick, asking nothing. original-ks.cfg is Anaconda's copy of the kickstart
  # it was handed — not anaconda-ks.cfg, which it writes out from the finished
  # configuration and which carries none of DSKY's own text.
  check "the kickstart's %post ran (/etc/dsky-provisioned)" "test -s /etc/dsky-provisioned"
  check "the kickstart DSKY wrote is the one Anaconda used" \
    "grep -q 'Generated for DSKY' /root/original-ks.cfg"
  check "the answers came from the initramfs, not a mounted partition" \
    "grep -q 'ks=file:/ks.cfg' /root/original-ks.cfg /var/log/anaconda/anaconda.log || grep -qr 'inst.ks=file' /var/log/anaconda/"
  check "package from %packages installed (vim-enhanced)" "rpm -q vim-enhanced"
else
  check "late-command ran (/etc/dsky-provisioned)" "test -s /etc/dsky-provisioned"
  check "answers were the ones DSKY wrote (autoinstall user-data mentions hello-world)" \
    "grep -rq hello-world /var/log/installer/"
  check "apt package from packages: installed (hello)" \
    "dpkg-query -W -f='\${Status}' hello | grep -q 'install ok installed'"
  wait_for_snapd $((15 * 60)) || true
  check_eventually $((10 * 60)) "snap from snaps: installed (hello-world)" "snap list hello-world" || true
fi
if [ "$IDENTITY" = true ]; then
  check "account from identity: exists (dsky)" "id dsky"
fi
target=multi-user
[ "$KIND" = desktop ] && target=graphical
check "installed system reached $target.target" "systemctl is-active $target.target"

ssh_run "systemd-analyze 2>/dev/null; uname -a; lsb_release -ds" >"$W/system.txt" 2>&1 || true
ssh_run poweroff >/dev/null 2>&1 || true
stop_vm
exit $fail
