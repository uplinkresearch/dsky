package agentbin

import (
	"os"
	"path/filepath"
	"testing"
)

// The agent is compiled separately and embedded here, so it is possible to
// change the agent, build DSKY, and ship media carrying the *previous* agent.
// That is not theoretical: it happened, and the machine it produced installed
// Windows, signed itself in, did nothing, and left no explanation — the agent
// had refused a manifest with a field it had never heard of and exited before
// it opened its log.
//
// The release workflow always runs ./build-agent.sh first, so released media
// is never stale. This is the guard for everybody else.
func TestTheEmbeddedAgentIsNotOlderThanTheAgent(t *testing.T) {
	if !Available(AMD64) {
		t.Skip("no agent embedded (./build-agent.sh) — nothing to be stale")
	}
	info, err := os.Stat(filepath.Join("bin", "dsky-agent-"+string(AMD64)+".exe.gz"))
	if err != nil {
		t.Fatal(err)
	}
	embedded := info.ModTime()

	src, err := os.ReadDir(filepath.Join("..", "agent"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range src {
		if e.IsDir() || filepath.Ext(e.Name()) != ".go" {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if fi.ModTime().After(embedded) {
			t.Fatalf("internal/agent/%s was changed after the embedded agent was built "+
				"(%s vs %s).\n\nMedia built now would carry the older agent, and a manifest it "+
				"cannot read stops first boot dead. Run ./build-agent.sh.",
				e.Name(), fi.ModTime().Format("15:04:05"), embedded.Format("15:04:05"))
		}
	}
}
