package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Scaffold creates a new workspace at dir for the named org, with example
// recipes to learn from. Refuses to overwrite existing files.
func Scaffold(dir, orgName string) error {
	return scaffold(dir, orgName, true)
}

// ScaffoldEmpty creates a workspace with no example recipes, for when one is
// made on somebody's behalf to hold a recipe they just saved: their list
// should show what they saved, not two examples they never asked for.
func ScaffoldEmpty(dir, orgName string) error {
	return scaffold(dir, orgName, false)
}

func scaffold(dir, orgName string, examples bool) error {
	if orgName == "" {
		return fmt.Errorf("org name is required")
	}
	orgID := slugify(orgName)
	files := map[string]string{
		"workspace.yaml":                          fmt.Sprintf(scaffoldWorkspaceYAML, orgName, orgID),
		".gitignore":                              scaffoldGitignore,
		"README.md":                               fmt.Sprintf(scaffoldReadme, orgName),
		"vars.local.yaml":                         scaffoldVarsLocal,
		"templates/autounattend.xml.tmpl":         scaffoldUnattend,
		"templates/autoinstall.yaml.tmpl":         scaffoldAutoinstall,
		"recipes/example-win11.yaml":              scaffoldWinRecipe,
		"recipes/example-ubuntu-autoinstall.yaml": scaffoldUbuntuRecipe,
		"manifests/ubuntu-24.04-iso.yaml":         scaffoldUbuntuManifest,
		"payload/.gitkeep":                        "",
	}
	if !examples {
		delete(files, "recipes/example-win11.yaml")
		delete(files, "recipes/example-ubuntu-autoinstall.yaml")
		delete(files, "manifests/ubuntu-24.04-iso.yaml")
		files["recipes/.gitkeep"] = ""
		files["manifests/.gitkeep"] = ""
	}
	for rel := range files {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err == nil {
			return fmt.Errorf("%s already exists — refusing to overwrite (init only creates fresh workspaces)", rel)
		}
	}
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), fileMode(rel)); err != nil {
			return err
		}
	}
	return nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// SecretsFile is the one file in a workspace whose purpose is holding
// secrets: the domain password, the administrator's, the Wi-Fi passphrase.
// Everything else in a workspace is meant to be read, committed and shared.
const SecretsFile = "vars.local.yaml"

// fileMode is how a scaffolded file is written. The secrets file is the
// exception: it was written 0644 like the README, so on any machine with more
// than one account -- a shared bench, a jump box, a technician's laptop that
// somebody else also signs in to -- every one of those accounts could read the
// domain password out of it. Being gitignored keeps it out of a repository and
// does nothing about the machine it sits on.
func fileMode(rel string) os.FileMode {
	if rel == SecretsFile {
		return 0o600
	}
	return 0o644
}

func slugify(s string) string {
	s = nonSlug.ReplaceAllString(strings.ToLower(s), "-")
	return strings.Trim(s, "-")
}

const scaffoldWorkspaceYAML = `version: 1
org:
  name: %q
  id: %q
defaults:
  locale: en-US
vars:
  # Workspace-wide template vars; recipes and vars.local.yaml override.
  admin_user: user
  admin_display_name: User
  admin_password: ""
  computer_name: "*"
`

const scaffoldGitignore = `# Machine-local secrets — never commit.
vars.local.yaml
# Download remnants.
*.part
`

const scaffoldVarsLocal = `# Machine-local values referenced as ${var:name} in recipes.
# This file is gitignored; put secrets here.
# admin_password: "hunter2"
# Ubuntu autoinstall password (SHA-512 crypt; openssl passwd -6 '...').
# This default is the hash of "changeme" — replace it before real use.
admin_password_hash: "$6$dsky$5S/Z2ILVSvNrtVKzqEEs6y6XvT9KctshEw9erjQirY7oY.lT94jPlSUC.iapQs1.ENfJAnoaqHX4iXAqWN2l81"
`

const scaffoldReadme = `# %s — DSKY workspace

Recipes, templates, and pinned-source manifests for building bootable
installation USB media with DSKY.

- ` + "`dsky recipes list`" + ` - what can be built
- ` + "`dsky sources pull <id>`" + ` - fetch a pinned source into the local library
- ` + "`dsky sources import <id> <file>`" + ` - add a manually-downloaded file (e.g. a Windows ISO)
- ` + "`dsky build <recipe>`" + ` - compose a bootable image
- ` + "`dsky devices`" + ` / ` + "`dsky flash <recipe> <device>`" + ` - write a USB stick

Multi-gigabyte binaries never live in this repo: manifests pin url + sha256
so any machine can re-fetch them.
`

// scaffoldUnattend is a production-proven unattended install: wipes disk 0,
// forces the edition via a generic key, creates a local admin, auto-logs-on
// once, and runs the generated first-boot script via FirstLogonCommands
// (SetupComplete.cmd is skipped under firmware OEM keys — do not use it).
const scaffoldUnattend = `<?xml version="1.0" encoding="utf-8"?>
<unattend xmlns="urn:schemas-microsoft-com:unattend" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">

  <settings pass="windowsPE">
    <component name="Microsoft-Windows-International-Core-WinPE" processorArchitecture="amd64"
               publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <SetupUILanguage><UILanguage>{{.Vars.locale}}</UILanguage></SetupUILanguage>
      <InputLocale>{{.Vars.locale}}</InputLocale>
      <SystemLocale>{{.Vars.locale}}</SystemLocale>
      <UILanguage>{{.Vars.locale}}</UILanguage>
      <UserLocale>{{.Vars.locale}}</UserLocale>
    </component>
    <component name="Microsoft-Windows-Setup" processorArchitecture="amd64"
               publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <DiskConfiguration>
        <!-- DANGER: wipes disk 0 without prompting. That is the point. -->
        <Disk wcm:action="add" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
          <DiskID>0</DiskID>
          <WillWipeDisk>true</WillWipeDisk>
          <CreatePartitions>
            <CreatePartition wcm:action="add">
              <Order>1</Order><Type>EFI</Type><Size>300</Size>
            </CreatePartition>
            <CreatePartition wcm:action="add">
              <Order>2</Order><Type>MSR</Type><Size>16</Size>
            </CreatePartition>
            <CreatePartition wcm:action="add">
              <Order>3</Order><Type>Primary</Type><Extend>true</Extend>
            </CreatePartition>
          </CreatePartitions>
          <ModifyPartitions>
            <ModifyPartition wcm:action="add">
              <Order>1</Order><PartitionID>1</PartitionID><Format>FAT32</Format><Label>System</Label>
            </ModifyPartition>
            <ModifyPartition wcm:action="add">
              <Order>2</Order><PartitionID>3</PartitionID><Format>NTFS</Format><Label>Windows</Label>
            </ModifyPartition>
          </ModifyPartitions>
        </Disk>
      </DiskConfiguration>
      <ImageInstall>
        <OSImage>
          <InstallTo>
            <DiskID>0</DiskID>
            <PartitionID>3</PartitionID>
          </InstallTo>
        </OSImage>
      </ImageInstall>
      <UserData>
        <AcceptEula>true</AcceptEula>
        <ProductKey>
          <!-- Generic edition-select key: forces the edition even when the
               firmware carries an OEM key for another one. Selects only;
               activation needs a real license. -->
          <Key>{{.Vars.edition_key}}</Key>
        </ProductKey>
      </UserData>
    </component>
  </settings>

  <settings pass="specialize">
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64"
               publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      {{if eq .Vars.domain_mode "offline"}}
      <!-- No ComputerName on an offline join, deliberately.
           djoin /provision creates the computer account for ONE named machine
           and the blob carries that name. Setting a name here too - and the
           default is "*", meaning random - renames the machine to something
           its own computer account does not match, which breaks the trust
           relationship the join just established.
           For the same reason do not add UserData/FullName: on Windows 11
           24H2 that silently overrides the blob's name as well.
           The component stays, empty: the specialize Shell-Setup component is
           documented as needing to exist. -->
      {{else}}
      <ComputerName>{{.Vars.computer_name}}</ComputerName>
      {{end}}
    </component>
    {{if eq .Vars.domain_mode "offline"}}
    <!-- Offline domain join (windows.domain.blob). The computer account was
         created in advance by djoin /provision, and this blob carries its
         machine-account password, so no user credential appears here and no
         domain controller has to be reachable while Setup runs. The blob
         belongs to exactly one machine, and is as sensitive as a password.

         Provisioning and Credentials are mutually exclusive - Provisioning
         wins if both appear, so only one is ever emitted. -->
    <component name="Microsoft-Windows-UnattendedJoin" processorArchitecture="amd64"
               publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <Identification>
        <Provisioning>
          <AccountData>{{xml .Vars.domain_odj_blob}}</AccountData>
        </Provisioning>
      </Identification>
    </component>
    {{else if eq .Vars.domain_mode "credentialed"}}
    <!-- Credentialed domain join (windows.domain.join).

         The password below is CLEARTEXT and cannot be otherwise: the
         PlainText/base64 obfuscation local-account passwords can use does not
         exist for this element. Nothing removes this file from the USB either
         - Setup copies it to Panther and scrubs the copy, never the media. So
         the stick carries a live domain credential for as long as it exists.
         Use an account delegated "Create Computer Objects" on the target OU
         and nothing more.

         Credentials/Domain authenticates the account; JoinDomain is the domain
         being joined. -->
    <component name="Microsoft-Windows-UnattendedJoin" processorArchitecture="amd64"
               publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <Identification>
        <Credentials>
          <Domain>{{xml .Vars.domain_account_domain}}</Domain>
          <Password>{{xml .Vars.domain_password}}</Password>
          <Username>{{xml .Vars.domain_user}}</Username>
        </Credentials>
        <JoinDomain>{{xml .Vars.domain_join}}</JoinDomain>
        {{if .Vars.domain_ou}}<MachineObjectOU>{{xml .Vars.domain_ou}}</MachineObjectOU>{{end}}
        <!-- Deliberately no DebugJoin element: its purpose is to break
             into a kernel debugger on failure and Microsoft says to leave it
             unmodified. The first-boot check already captures netsetup.log and
             both UnattendGC logs, which is the diagnostic that matters. -->
      </Identification>
    </component>
    {{end}}
  </settings>

  <settings pass="oobeSystem">
    <component name="Microsoft-Windows-International-Core" processorArchitecture="amd64"
               publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <InputLocale>{{.Vars.locale}}</InputLocale>
      <SystemLocale>{{.Vars.locale}}</SystemLocale>
      <UILanguage>{{.Vars.locale}}</UILanguage>
      <UserLocale>{{.Vars.locale}}</UserLocale>
    </component>
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64"
               publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <OOBE>
        <HideEULAPage>true</HideEULAPage>
        <HideOEMRegistrationScreen>true</HideOEMRegistrationScreen>
        <HideOnlineAccountScreens>true</HideOnlineAccountScreens>
        <HideWirelessSetupInOOBE>true</HideWirelessSetupInOOBE>
        <ProtectYourPC>3</ProtectYourPC>
      </OOBE>
      <UserAccounts>
        <LocalAccounts>
          <LocalAccount wcm:action="add" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
            <Name>{{xml .Vars.admin_user}}</Name>
            <Group>Administrators</Group>
            <DisplayName>{{xml .Vars.admin_display_name}}</DisplayName>
            <Password>
              <Value>{{xml .Vars.admin_password}}</Value>
              <PlainText>true</PlainText>
            </Password>
          </LocalAccount>
        </LocalAccounts>
      </UserAccounts>
      <!-- One auto-logon so FirstLogonCommands fire hands-off. -->
      <AutoLogon>
        <Enabled>true</Enabled>
        <LogonCount>1</LogonCount>
        <Username>{{xml .Vars.admin_user}}</Username>
        <Password>
          <Value>{{xml .Vars.admin_password}}</Value>
          <PlainText>true</PlainText>
        </Password>
      </AutoLogon>
      <FirstLogonCommands>
        <SynchronousCommand wcm:action="add" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
          <Order>1</Order>
          <CommandLine>cmd /c C:\Windows\Setup\Scripts\firstboot.cmd</CommandLine>
          <Description>Run first-boot drivers and payload</Description>
        </SynchronousCommand>
      </FirstLogonCommands>
    </component>
  </settings>

</unattend>
`

const scaffoldWinRecipe = `version: 1
id: example-win11
name: "Example Win11 Pro unattended stick"

os:
  # windows-11 is the catalog's official Windows 11 ISO: building fetches it
  # from Microsoft, uses a Win11_*.iso already in Downloads, or reuses the one
  # DSKY already downloaded. Or name your own manifest here, or switch to
  # source_mode: tree with tree_path pointing at a captured master stick.
  source: windows-11
  type: windows
  source_mode: auto

target:
  scheme: mbr
  filesystem: fat32
  volume_label: ESD-USB
  size: auto
  min_stick: 8GiB
  boot: uefi-only

windows:
  ei_cfg: { edition: Professional, channel: Retail, vl: false }
  unattend:
    template: templates/autounattend.xml.tmpl
    vars:
      # Generic Win11 Pro edition-select key (public, selects edition only).
      edition_key: VK7JG-NPHTM-C97JM-9MPGT-3V66T
      locale: en-US
  # winpe_drivers: [intel-vmd-pack]   # boot-critical storage/NIC -> $WinpeDriver$/
  driver_packs: []
  #  - { ref: intel-lan-pack, install: pnputil-sweep }
  #  - { ref: intel-sst-cab, install: expand-then-sweep }
  #  - { ref: intel-wifi-exe, install: exe, args: ["-q", "-s"] }
  payload: []
  #  - { ref: my-rmm-agent-msi }
  firstboot:
    mode: generate
    steps:
      - drivers
  #    - wait: 10s
  #    - msi: { ref: my-rmm-agent-msi, args: ["/qn"] }

flash:
  verify: readback-sha256
`

const scaffoldUbuntuManifest = `id: ubuntu-24.04-iso
kind: os-image
format: iso
url: https://releases.ubuntu.com/24.04/ubuntu-24.04.5-live-server-amd64.iso
# Pinned from https://releases.ubuntu.com/24.04/SHA256SUMS. When a new point
# release replaces the file, update url + sha256 together from that list.
sha256: 97f3d7ffb032c3eb3b23d2c8be9cc76e60c2c1f2c0146ba5ba9fe01cafae0fd8
notes: "Ubuntu 24.04 LTS live server; hybrid ISO"
`

// scaffoldAutoinstall is a subiquity autoinstall (cloud-config) that
// installs unattended: hostname/user from vars, SSH server, whole-disk
// direct layout, then reboots. The password is a SHA-512 crypt hash.
const scaffoldAutoinstall = `#cloud-config
# Ubuntu autoinstall (subiquity). Rendered by DSKY into the CIDATA
# partition; the GRUB menu is patched so this runs with nobody present.
autoinstall:
  version: 1
  locale: {{.Vars.locale}}
  keyboard:
    layout: us
  identity:
    hostname: {{.Vars.hostname}}
    username: {{.Vars.admin_user}}
    # SHA-512 crypt hash. Generate: openssl passwd -6 'yourpassword'
    password: "{{.Vars.admin_password_hash}}"
  ssh:
    install-server: true
    allow-pw: true
  storage:
    layout:
      # lvm is what Ubuntu Server's own guided install chooses, and what a
      # stick built from the catalog now writes, so a server built either way
      # comes out the same. "direct" gives a plain partition instead, which is
      # what Ubuntu Desktop does -- simpler, and cannot be grown or snapshotted
      # later without moving the data off first.
      name: lvm
      # Fill the volume group. Ubuntu's default leaves about half of it spare
      # for snapshots; on a machine being handed to somebody, a 40 GB disk
      # showing an 18 GB root reads as a fault. Drop this line to get Ubuntu's
      # reserve back.
      sizing-policy: all
  packages:
    - openssh-server
  late-commands:
    - echo "provisioned by DSKY ({{.Org.Name}})" > /target/etc/dsky-provisioned
  shutdown: reboot
`

const scaffoldUbuntuRecipe = `version: 1
id: example-ubuntu-autoinstall
name: "Ubuntu 24.04 server — zero-touch autoinstall"

os:
  type: linux-iso
  source: ubuntu-24.04-iso

target:
  min_stick: 8GiB
  boot: uefi-only

linux:
  autoinstall:
    user_data: templates/autoinstall.yaml.tmpl
    vars:
      hostname: appliance
      # Default hash is for the password "changeme" — override in
      # vars.local.yaml (gitignored) with: openssl passwd -6 'yourpassword'
      admin_password_hash: "${var:admin_password_hash}"

firmware_notes: >-
  Boots UEFI. DANGER: the autoinstall wipes the first disk without asking.

flash:
  verify: readback-sha256
`
