package agent

import (
	"strings"
	"testing"
)

// A queue from a print server and a printer with its own address are added by
// different people, in different places, and getting that wrong is the
// difference between a printer that works and one nobody can print to.
//
// A shared queue is a connection: it belongs to the person, and their session
// pulls the driver from the server. Adding it as the local administrator at
// first boot would put it in an account nobody uses. A direct printer is the
// machine's, and needs a driver already installed.
func TestSharedPrintersBelongToThePersonAndDirectOnesToTheMachine(t *testing.T) {
	m := &Manifest{Printers: []Printer{
		{Name: "Front Desk", SharedPath: `\\LAB-DC01\FrontDesk`},
		{Name: "Back Office HP", IP: "10.10.10.50", DriverName: "HP Universal Printing PCL 6"},
	}}
	shared := m.sharedPrinters()
	if len(shared) != 1 || shared[0].Name != "Front Desk" {
		t.Fatalf("shared: %+v", shared)
	}
	if !m.Printers[0].Shared() || m.Printers[1].Shared() {
		t.Errorf("the two kinds are not being told apart: %+v", m.Printers)
	}
}

// The per-user script adds a shared queue, maps a drive and writes settings
// from one typed data file — and it is the same script on every machine.
func TestThePerUserScriptHandlesEachKindOfLine(t *testing.T) {
	for _, want := range []string{
		`if /i "%~1"=="reg"`,
		`if /i "%~1"=="printer"`,
		`if /i "%~1"=="drive"`,
		"printui.dll,PrintUIEntry /in", // a connection, installed into the session
		"net use",
	} {
		if !strings.Contains(userSetupScript, want) {
			t.Errorf("the per-user script does not handle %q:\n%s", want, userSetupScript)
		}
	}
}

// A single quote in a printer's name must not end the PowerShell string it is
// passed in. Practices name printers after rooms and people.
func TestAPrinterNameWithAQuoteIsSafe(t *testing.T) {
	if got := psQuote("Dr O'Brien's Room"); got != "Dr O''Brien''s Room" {
		t.Errorf("psQuote = %q", got)
	}
}
