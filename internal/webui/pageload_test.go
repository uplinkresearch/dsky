package webui

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The portal's JavaScript is never run by anything but a person.
//
// It was checked for syntax, and syntax was not the problem: the program
// picker referred to a variable that had been renamed months earlier, threw
// before it drew a single row, and found nothing — for every operating system,
// in all four places the picker appears. The exception unwound into an event
// handler where nothing caught it, so the page showed no error; the list was
// simply always empty. It shipped in nine releases.
//
// So this loads the page the way a browser does — a small generic DOM, the
// real script, its real startup — and then uses it. Anything the page's own
// code throws, on load or in the picker, fails here.
//
// It is not a rendering test and makes no claim about how anything looks.
// It answers one question: does this page work at all?
func TestThePageLoadsAndItsPickerFindsPrograms(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on this machine; the page-load check needs one")
	}
	page, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	scripts := regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`).FindAllStringSubmatch(string(page), -1)
	if len(scripts) == 0 {
		t.Fatal("the page has no script; if it moved to a file, point this test at it")
	}
	var src strings.Builder
	for _, m := range scripts {
		src.WriteString(m[1])
		src.WriteString("\n")
	}

	shim, err := os.ReadFile(filepath.Join("testdata", "pageload.js"))
	if err != nil {
		t.Fatal(err)
	}
	// The driver runs in the page's own scope, so it reaches what the page
	// declares with let and const — which is everything.
	harness := string(shim) + "\n" + src.String() + "\n" + driver
	dir := t.TempDir()
	file := filepath.Join(dir, "page.js")
	if err := os.WriteFile(file, []byte(harness), 0o644); err != nil {
		t.Fatal(err)
	}

	fixture, err := json.Marshal(map[string]any{"/api/state": pageFixture()})
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, file, string(fixture)).CombinedOutput()
	if err != nil {
		t.Fatalf("the page did not load:\n%s", trimNode(string(out)))
	}
	var got struct {
		Browse  int      `json:"browse"`
		Named   int      `json:"named"`
		Mine    int      `json:"mine"`
		Targets []string `json:"targets"`
		Error   string   `json:"error"`
	}
	if err := json.Unmarshal([]byte(lastLine(string(out))), &got); err != nil {
		t.Fatalf("the check printed something unreadable:\n%s", trimNode(string(out)))
	}
	if got.Error != "" {
		t.Fatalf("the page threw while its picker was used: %s", got.Error)
	}
	// Opened with nothing typed, the picker lists everything to browse.
	if got.Browse < 2 {
		t.Errorf("the picker listed %d program(s) with an empty box; it should list them all", got.Browse)
	}
	// Typed, it finds a program by name...
	if got.Named != 1 {
		t.Errorf("searching for a program by name found %d", got.Named)
	}
	// ...and an installer the operator supplied, which is the one nobody else
	// can work around: it is not in winget and cannot be typed as an id.
	if got.Mine != 1 {
		t.Errorf("searching for the operator's own installer found %d", got.Mine)
	}
	// Every place the picker appears, not just the one somebody reported.
	if len(got.Targets) != 4 {
		t.Errorf("only %d of the picker's four forms worked: %v", len(got.Targets), got.Targets)
	}
}

// pageFixture is the server's answer, reduced to what the picker needs.
func pageFixture() map[string]any {
	return map[string]any{
		"org": "Test", "has_workspace": true, "host_os": "linux",
		"recent": []any{}, "recipes": []any{}, "sources": []any{}, "devices": []any{},
		"artifacts": []any{}, "downloads": []any{}, "catalog": []any{}, "app_sets": []any{},
		"not_in_winget": []any{}, "guides_seen": []string{},
		"apps": []map[string]any{
			{"id": "brave", "name": "Brave", "category": "Browsers", "winget": "Brave.Brave",
				"windows": true, "ubuntu": true, "fedora": true},
			{"id": "vlc", "name": "VLC", "category": "Media", "winget": "VideoLAN.VLC",
				"windows": true, "ubuntu": true, "fedora": true},
			{"id": "brecsc", "name": "BRECsc.msi", "category": "Your installers",
				"windows": true, "mine": true},
		},
	}
}

const driver = `
// ---- the test's own use of the page ----------------------------------------
(() => {
  const out = { targets: [], browse: 0, named: 0, mine: 0, error: "" };
  const count = (wrap, word) => {
    const seen = [];
    const walk = e => { if ((e.textContent || "").includes(word)) seen.push(e);
                        (e.children || []).forEach(walk); };
    walk(wrap);
    // The deepest match is the row; its parents contain it too.
    return seen.filter(e => !(e.children || []).some(c => (c.textContent || "").includes(word))).length;
  };
  try {
    state = JSON.parse(process.argv[2])["/api/state"];
    for (const target of ["windows", "ubuntu", "fedora", "payload"]) {
      const o = appsOpt(target);
      const box = o.wrap.querySelectorAll ? null : null;
      // The search box is the one input the picker makes.
      const find = e => { if (e.tagName === "INPUT" && e.type === "search") return e;
                          for (const c of e.children || []) { const r = find(c); if (r) return r; } return null; };
      const search = find(o.wrap);
      if (!search) throw new Error(target + ": the picker has no search box");
      const type = q => { search.value = q; search.dispatchEvent(new Event("input")); };
      type("");
      if (target === "windows") out.browse = count(o.wrap, "Brave") + count(o.wrap, "VLC");
      type("brave");
      if (target === "windows") out.named = count(o.wrap, "Brave");
      type("BREC");
      if (target === "windows") out.mine = count(o.wrap, "BRECsc.msi");
      out.targets.push(target);
    }
  } catch (e) {
    out.error = (e && e.message) || String(e);
  }
  console.log(JSON.stringify(out));
})();
`

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

func trimNode(s string) string {
	if len(s) > 2000 {
		return s[:2000] + "\n…"
	}
	return s
}
