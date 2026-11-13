package compose

import (
	"github.com/uplinkresearch/dsky/internal/recipe"
)

// Naming the machine after its serial number (recipe.SerialName has the why).
//
// The answer file runs the naming script in the specialize pass, straight
// after the command that copies DSKY's first-boot files off the stick, because
// that copy is what puts the script on the machine. withOEMCopy puts its own
// command first and renumbers the rest, so adding this one before it is what
// leaves the copy at Order 1 and the naming at Order 2.
//
// Specialize is also where Windows applies the answer file's own ComputerName.
// The build sets that to "*", so whichever runs last the result is never a
// fixed name shared by every machine from the stick; and the post-install
// check fails a machine whose name did not come out PC-something, so a name
// that did not take is seen rather than shipped.

const serialNameDescription = "DSKY: name this computer after its serial number"

// The script's path is fixed by where withOEMCopy puts the Scripts folder.
const serialNameCommand = `powershell.exe -NoProfile -ExecutionPolicy Bypass -File C:\Windows\Setup\Scripts\` +
	recipe.SerialNameScriptName

func withSerialName(unattend string) (string, error) {
	return firstSpecializeCommand(unattend, serialNameDescription, serialNameCommand)
}
