package recipe

import (
	"regexp"
	"strconv"
	"strings"
)

// Naming a machine after its serial number.
//
// A stick that images twenty machines has to give them twenty names, and two
// sticks running at once must not hand out the same one. A counter cannot do
// that -- each stick only knows its own count -- but every machine already
// carries a name nobody else has: the serial number its maker printed on it.
// So the machine is called PC-<serial>, worked out on the machine itself while
// Windows installs, and the stick needs no list and no coordination.
//
// The name is set in the specialize pass, by a script the answer file runs
// right after DSKY's first-boot files are copied off the stick. Specialize is
// followed by a restart before Windows' first sign-in, which is when a new
// name takes effect, so this costs no restart of its own.
//
// Two things can make a serial number useless as a name, and both are common
// on the machines a bench sees:
//
//   - Placeholders. Boards from smaller makers, and nearly every virtual
//     machine, report "To Be Filled By O.E.M.", "Default string", "System
//     Serial Number", "0" or nothing at all. Twenty machines named
//     PC-DEFAULTSTRING is the failure this feature exists to prevent, so a
//     placeholder falls back to the first network card's hardware address,
//     which is unique too.
//   - Length. Windows refuses a computer name over 15 characters. "PC-" takes
//     three, so a longer serial keeps its LAST 12: the end of a serial is the
//     part that differs between machines of one batch.
//
// When neither is usable the script leaves the name Windows chose and says so
// in its log. It never fails Setup: a machine with the wrong name is a nuisance,
// and a machine that did not install is a lost morning.
//
// SerialName is the same rule in Go. The script is what runs; this is what the
// tests check it against, the way the model gate's matcher is.

// SerialNameScriptName is the script's filename under Scripts\.
const SerialNameScriptName = "dsky-name.ps1"

// SerialNamePrefix starts every name this produces.
const SerialNamePrefix = "PC-"

// serialNameMax is what is left of Windows' 15-character limit after the prefix.
const serialNameMax = 15 - len(SerialNamePrefix)

// serialPlaceholders are values makers and hypervisors put where a serial
// number should be, compared after cleaning (upper case, letters and digits
// only).
var serialPlaceholders = []string{
	"TOBEFILLEDBYOEM", "DEFAULTSTRING", "SYSTEMSERIALNUMBER", "CHASSISSERIALNUMBER",
	"SERIALNUMBER", "NONE", "NA", "NOTAPPLICABLE", "NOTSPECIFIED", "NOTAVAILABLE",
	"INVALID", "UNKNOWN", "EMPTY", "OEM", "0123456789", "123456789", "1234567890",
}

var notSerialChar = regexp.MustCompile(`[^A-Z0-9]`)

func cleanSerial(s string) string {
	return notSerialChar.ReplaceAllString(strings.ToUpper(s), "")
}

// usableSerial rejects placeholders, values too short to be anybody's serial,
// and one character repeated (00000000, FFFFFFFFFFFF).
func usableSerial(c string) bool {
	if len(c) < 4 {
		return false
	}
	for _, p := range serialPlaceholders {
		if c == p {
			return false
		}
	}
	return strings.Trim(c, c[:1]) != ""
}

// SerialName is the name a machine with this serial number and this network
// card address gets, and which of the two it came from ("serial" or "mac").
// Empty when neither is usable.
func SerialName(serial, mac string) (name, from string) {
	c, from := cleanSerial(serial), "serial"
	if !usableSerial(c) {
		c, from = cleanSerial(mac), "mac"
		if !usableSerial(c) {
			return "", ""
		}
	}
	if len(c) > serialNameMax {
		c = c[len(c)-serialNameMax:]
	}
	return SerialNamePrefix + c, from
}

// SerialNameCase is one machine the script and SerialName must agree on.
type SerialNameCase struct {
	Serial, MAC, Want string
}

// SerialNameCases are shared by the Go test and the script's own test.
var SerialNameCases = []SerialNameCase{
	{"5CD1234XYZ", "", "PC-5CD1234XYZ"},                                        // HP
	{"7XK2LM3", "", "PC-7XK2LM3"},                                              // Dell service tag
	{"pf3abc12", "", "PC-PF3ABC12"},                                            // Lenovo, lower case
	{"  5CD-1234 XYZ ", "", "PC-5CD1234XYZ"},                                   // spaces and dashes
	{"0123456789012345", "", "PC-456789012345"},                                // too long: keep the end
	{"VMware-56 4d 1a 2b 3c 4d 5e 6f", "00:0C:29:AB:CD:EF", "PC-1A2B3C4D5E6F"}, // VMware's own serial is unique
	{"To Be Filled By O.E.M.", "00-1A-2B-3C-4D-5E", "PC-001A2B3C4D5E"},
	{"Default string", "00:1a:2b:3c:4d:5e", "PC-001A2B3C4D5E"},
	{"System Serial Number", "001A2B3C4D5E", "PC-001A2B3C4D5E"},
	{"0", "001A2B3C4D5E", "PC-001A2B3C4D5E"},
	{"00000000", "001A2B3C4D5E", "PC-001A2B3C4D5E"},
	{"", "001A2B3C4D5E", "PC-001A2B3C4D5E"},
	{"", "", ""},
	{"Default string", "00:00:00:00:00:00", ""},
}

// SerialNameScriptFile is the script as it is staged: CRLF, ASCII.
func SerialNameScriptFile() string { return toCRLF(SerialNameScript) }

// SerialNameScript sets the computer name to PC-<serial>. -CheckOnly with
// -Serial and -Mac prints the name it would set and changes nothing, which is
// how its test runs it.
var SerialNameScript = `# DSKY: name this computer PC-<serial number>.
# Generated by DSKY. Runs once, in the specialize pass of Windows Setup; the
# name takes effect at the restart that follows. It never fails Setup.
param([string]$Serial, [string]$Mac, [switch]$CheckOnly)

$log = Join-Path $PSScriptRoot 'dsky-name.log'
function Log([string]$m) {
  if (-not $CheckOnly) {
    try { Add-Content -Path $log -Value ('[' + (Get-Date -Format s) + '] ' + $m) } catch {}
  }
}

$placeholders = @(` + quotedList(serialPlaceholders) + `)
function Clean([string]$s) {
  if ($null -eq $s) { return '' }
  return ($s.ToUpperInvariant() -replace '[^A-Z0-9]', '')
}
function Usable([string]$c) {
  if ($c.Length -lt 4) { return $false }
  if ($placeholders -contains $c) { return $false }
  if ($c -match '^(.)\1*$') { return $false }
  return $true
}

try {
  if (-not $PSBoundParameters.ContainsKey('Serial')) {
    $Serial = ''
    try { $Serial = [string](Get-CimInstance Win32_BIOS).SerialNumber } catch {}
  }
  if (-not $PSBoundParameters.ContainsKey('Mac')) {
    $Mac = ''
    try {
      $nic = Get-CimInstance Win32_NetworkAdapter -Filter 'PhysicalAdapter=True' |
        Where-Object { $_.MACAddress } | Sort-Object Index | Select-Object -First 1
      if ($nic) { $Mac = [string]$nic.MACAddress }
    } catch {}
  }

  $c = Clean $Serial
  $from = 'serial number'
  if (-not (Usable $c)) {
    $c = Clean $Mac
    $from = 'network card address'
    if (-not (Usable $c)) {
      Log ("no usable serial number ('" + $Serial + "') or network card address ('" + $Mac + "'): keeping the name Windows chose")
      if ($CheckOnly) { 'none' }
      exit 0
    }
  }
  if ($c.Length -gt ` + strconv.Itoa(serialNameMax) + `) { $c = $c.Substring($c.Length - ` + strconv.Itoa(serialNameMax) + `) }
  $name = '` + SerialNamePrefix + `' + $c
  if ($CheckOnly) { $name; exit 0 }

  # The two values SetComputerNameEx writes: the name Windows takes at the
  # next start, and the host name the network stack takes with it.
  Set-ItemProperty -Path 'HKLM:\SYSTEM\CurrentControlSet\Control\ComputerName\ComputerName' -Name 'ComputerName' -Value $name
  Set-ItemProperty -Path 'HKLM:\SYSTEM\CurrentControlSet\Services\Tcpip\Parameters' -Name 'NV Hostname' -Value $name
  Log ('named this computer ' + $name + ' from its ' + $from + " ('" + $Serial + "', '" + $Mac + "')")
} catch {
  Log ('could not set the name: ' + $_)
}
exit 0
`

func quotedList(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = "'" + s + "'"
	}
	return strings.Join(q, ", ")
}
