package agentbin

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The agent is compiled separately and embedded here, so it is possible to
// change the agent, build DSKY, and ship media carrying the *previous* agent.
//
// That is not theoretical. It happened, and the machine it produced installed
// Windows, signed itself in, did nothing, and left no explanation: the agent
// had refused a manifest carrying a field it had never heard of, and exited
// before it opened its log. Every unit test passed, in this package and every
// other, because they all use the agent in this process rather than the one on
// the media.
//
// Compared by content rather than by modification time. Timestamps make this
// fire when gofmt rewrites a file nobody changed, and a guard that cries wolf
// is one people learn to ignore.
//
// The release workflow builds the agent first, so released media was never at
// risk. This is the guard for everybody working on it.
func TestTheEmbeddedAgentWasBuiltFromTheseSources(t *testing.T) {
	if !Available(AMD64) {
		t.Skip("no agent embedded (./build-agent.sh) — nothing to be stale")
	}
	want, err := os.ReadFile(filepath.Join("bin", "sources.sha256"))
	if err != nil {
		t.Skip("this agent was embedded before the check existed; ./build-agent.sh records it")
	}
	got, err := agentSourcesHash()
	if err != nil {
		t.Fatal(err)
	}
	if got != strings.TrimSpace(string(want)) {
		t.Fatalf("the embedded agent was built from different sources than the ones here.\n\n"+
			"Media built now would carry that older agent, and a manifest it cannot read stops\n"+
			"first boot dead — with no log on the machine until it is rebuilt. Run ./build-agent.sh.\n\n"+
			"  embedded: %s\n  sources:  %s", strings.TrimSpace(string(want)), got)
	}
}

// agentSourcesHash hashes what goes into the agent, the same way build-agent.sh
// does: every non-test .go file under internal/agent and cmd/dsky-agent, in
// sorted order, concatenated.
func agentSourcesHash() (string, error) {
	// Both the name build-agent.sh sees (from the repository root, which is
	// what it sorts by) and the path this test can open.
	type src struct{ key, path string }
	var files []src
	for _, d := range []struct{ key, dir string }{
		{"internal/agent", filepath.Join("..", "agent")},
		{"cmd/dsky-agent", filepath.Join("..", "..", "cmd", "dsky-agent")},
	} {
		entries, err := os.ReadDir(d.dir)
		if err != nil {
			return "", err
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".go" || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			files = append(files, src{d.key + "/" + e.Name(), filepath.Join(d.dir, e.Name())})
		}
	}
	// `find internal/agent cmd/dsky-agent ... | sort` sorts by the path from
	// the repository root, which puts cmd before internal -- not the order
	// the directories are listed in.
	sort.Slice(files, func(i, j int) bool { return files[i].key < files[j].key })
	h := sha256.New()
	for _, f := range files {
		b, err := os.ReadFile(f.path)
		if err != nil {
			return "", err
		}
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
