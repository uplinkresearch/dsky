package agent

import (
	"fmt"
	"time"
)

// StepPrinters is the step name compose puts in a manifest's step list.
const StepPrinters = "printers"

const stepPrinters = StepPrinters

// Printer is one queue from the old machine.
//
// The two kinds are not variations on a theme, and the difference decides who
// can add them. A queue shared from a print server is a connection: it belongs
// to the person, their session installs the driver from the server, and it is
// added by the per-user script rather than here. A printer with its own IP
// address is the machine's, and needs a driver already on the machine -- which
// on a new Windows 11 usually means an inbox one, or nothing.
type Printer struct {
	Name       string `json:"name"`
	SharedPath string `json:"shared_path,omitempty"`
	IP         string `json:"ip,omitempty"`
	Port       string `json:"port,omitempty"`
	DriverName string `json:"driver_name,omitempty"`
}

// Shared reports whether this queue comes from a print server.
func (p Printer) Shared() bool { return p.SharedPath != "" }

// MappedDrive is a drive letter somebody had. It belongs to the person, not
// the machine, so it is staged into the per-user script for the same reason
// their file-extension setting is.
type MappedDrive struct {
	Letter string `json:"letter"`
	UNC    string `json:"unc"`
}

// printersStep adds the queues that belong to the machine. The shared ones
// are not here: they are added at sign-in, by whoever signs in.
func (a *Agent) printersStep() {
	var direct []Printer
	for _, p := range a.Manifest.Printers {
		if !p.Shared() {
			direct = append(direct, p)
		}
	}
	if len(direct) == 0 {
		a.J.Info(stepPrinters, "no printers of this machine's own to add")
		return
	}
	a.UI.Detail("adding " + itoa(len(direct)) + " printer(s)")
	for _, p := range direct {
		a.addDirectPrinter(p)
	}
}

// addDirectPrinter adds one IP printer: its port, then the queue.
//
// This is the step most likely to fail on a real machine, and the failure is
// worth more than the success. The old PC had a driver somebody installed
// years ago; the new one has whatever Windows 11 ships. So say exactly what
// was missing and name the printer the way the plan names it, because the
// next thing that happens is a person installing that driver by hand.
func (a *Agent) addDirectPrinter(p Printer) {
	if p.IP == "" {
		a.J.Fail(stepPrinters, "%s has no address, so it cannot be added here", p.Name)
		return
	}
	port := p.Port
	if port == "" {
		port = "IP_" + p.IP
	}
	// Adding a port that already exists is an error, and an uninteresting
	// one; the printer add below is the step that matters.
	run(2*time.Minute, "powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command",
		fmt.Sprintf("Add-PrinterPort -Name '%s' -PrinterHostAddress '%s' -ErrorAction SilentlyContinue", psQuote(port), psQuote(p.IP)))

	if p.DriverName == "" {
		a.J.Fail(stepPrinters, "%s (%s) was not added: the plan does not say which driver it used, "+
			"so it has to be added by hand", p.Name, p.IP)
		return
	}
	r := run(3*time.Minute, "powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command",
		fmt.Sprintf("Add-Printer -Name '%s' -DriverName '%s' -PortName '%s'",
			psQuote(p.Name), psQuote(p.DriverName), psQuote(port)))
	if !r.ok() {
		a.J.FailDetail(stepPrinters,
			fmt.Sprintf("%s (%s) was not added — its driver %q is not on this machine, so install the "+
				"driver and add the printer by hand", p.Name, p.IP, p.DriverName),
			trimOut(r.Out))
		return
	}
	a.J.Info(stepPrinters, "added %s (%s)", p.Name, p.IP)
}

// psQuote makes a value safe inside a single-quoted PowerShell string, where
// the only character that matters is the quote itself.
func psQuote(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\'' {
			out = append(out, '\'')
		}
		out = append(out, r)
	}
	return string(out)
}
