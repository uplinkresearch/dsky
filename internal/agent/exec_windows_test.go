package agent

import (
	"context"
	"testing"
)

// Every program the agent starts must start without a window of its own. The
// agent is linked for the GUI subsystem and so has no console to lend a child;
// a winget or pnputil started without this would be given a new console, and
// a run would flash windows at whoever is sitting at the machine.
func TestTheProgramsTheAgentStartsHaveNoWindow(t *testing.T) {
	c := command(context.Background(), "winget", "install")
	if c.SysProcAttr == nil || !c.SysProcAttr.HideWindow {
		t.Errorf("SysProcAttr = %+v; a console window would open for this", c.SysProcAttr)
	}
	const createNoWindow = 0x08000000
	if c.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Errorf("CreationFlags = %#x, want CREATE_NO_WINDOW set", c.SysProcAttr.CreationFlags)
	}
}
