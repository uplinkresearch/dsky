//go:build windows

package agent

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Where the agent says things when it is not the status window saying them.
//
// The agent is linked for the GUI subsystem (build-agent.sh passes
// -H=windowsgui). It has to be: a payload is one file somebody double-clicks,
// and a console-subsystem program double-clicked from Explorer is given a
// console window, which then sits on the desktop behind the status window for
// the whole run. Somebody watching a payload install their programs should not
// also be watching an empty black window they did not ask for and can close by
// accident.
//
// But this program is a command line tool as well: `dsky-agent verify` prints
// a report, `scan` prints what it read off a machine somebody is standing at,
// and a remote tool runs `apply --quiet` and reads what comes back. A
// GUI-subsystem program starts with no standard output at all, so all of that
// would go nowhere. When it was started from a console -- a PowerShell window,
// a script, a remote tool -- that console is attached to and used, and those
// commands behave exactly as they did.
//
// When there is no console, an error has nowhere to go, and an agent that
// refuses to run and says nothing is the failure this whole program is written
// against: a machine that looks untouched, with no trace of why. So it goes in
// a message box instead. Once, at the end, for the error that stopped the run.

// attachParentProcess is ATTACH_PARENT_PROCESS: (DWORD)-1, the console of
// whatever started this program.
const attachParentProcess = ^uintptr(0)

// mbIconError is MB_OK | MB_ICONERROR | MB_SETFOREGROUND, so the box is in
// front of the person rather than behind whatever they were looking at.
const mbIconError = 0x00000000 | 0x00000010 | 0x00010000

var pMessageBoxW = user32.NewProc("MessageBoxW")

// haveConsole records whether the attach found one, so an error knows whether
// there is anywhere to print it.
var haveConsole bool

// UseParentConsole attaches to the console of whatever started this program,
// if there is one, and points the standard streams at it.
//
// A stream that was already handed over is left exactly as it was. That is
// not a detail: a script running `dsky-agent verify > report.txt`, or a
// remote tool reading the run through a pipe, gave this program somewhere to
// write, and replacing it with the console would send their report to a
// window nobody asked for and leave their file empty.
//
// Best effort throughout: every failure here means "carry on with no
// console", which is the ordinary case for a double-clicked payload rather
// than a fault.
func UseParentConsole() {
	attach := kernel32.NewProc("AttachConsole")
	if ok, _, _ := attach.Call(attachParentProcess); ok == 0 {
		// No console to attach to. There may still be a pipe or a file.
		haveConsole = handed(windows.STD_ERROR_HANDLE)
		return
	}
	for _, s := range []struct {
		std  uint32
		name string
		to   **os.File
	}{
		{windows.STD_OUTPUT_HANDLE, "CONOUT$", &os.Stdout},
		{windows.STD_ERROR_HANDLE, "CONOUT$", &os.Stderr},
		{windows.STD_INPUT_HANDLE, "CONIN$", &os.Stdin},
	} {
		if handed(s.std) {
			continue
		}
		if f := openConsole(s.name, s.std); f != nil {
			*s.to = f
		}
	}
	haveConsole = handed(windows.STD_ERROR_HANDLE)
}

// handed reports whether this process was already given that standard
// stream. A GUI-subsystem program started from Explorer has none; one started
// from a shell, a script or a remote tool has whatever that gave it.
func handed(std uint32) bool {
	h, err := windows.GetStdHandle(std)
	return err == nil && h != 0 && h != windows.InvalidHandle
}

// openConsole opens one of the attached console's own streams and makes it
// this process's standard handle, so anything that asks Windows for the
// handle rather than reading os.Stdout finds it too.
func openConsole(name string, std uint32) *os.File {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil
	}
	windows.SetStdHandle(std, h)
	return os.NewFile(uintptr(h), name)
}

// SayFatal reports the error that stopped the run, wherever there is to
// report it: the console this was started from, or a message box when there
// is none, because a payload that was double-clicked and did nothing must
// still say why.
func SayFatal(err error) {
	if haveConsole {
		fmt.Fprintln(os.Stderr, "dsky-agent:", err)
		return
	}
	text, terr := windows.UTF16PtrFromString(err.Error())
	caption, cerr := windows.UTF16PtrFromString("DSKY payload")
	if terr != nil || cerr != nil {
		return
	}
	pMessageBoxW.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(caption)), mbIconError)
}
