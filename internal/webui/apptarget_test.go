package webui

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// The dialog has to be told which program list an entry takes, not left to
// work it out. It used to infer "Ubuntu" from whether third-party drivers were
// offered — true of Ubuntu today, and quietly the wrong list for the next
// entry that offers neither.
func TestTheStateSaysWhichProgramListEachOSTakes(t *testing.T) {
	s := testServer(t)
	req := httptest.NewRequest("GET", "/api/state", nil)
	req.Host = "127.0.0.1:8931"
	req.Header.Set("X-DSKY-Token", "sekrit")
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, req)

	var state struct {
		Catalog []struct {
			ID                string `json:"id"`
			Programs          bool   `json:"programs"`
			AppTarget         string `json:"app_target"`
			ThirdPartyDrivers bool   `json:"third_party_drivers"`
		} `json:"catalog"`
		Apps []struct {
			ID     string `json:"id"`
			Ubuntu bool   `json:"ubuntu"`
			Fedora bool   `json:"fedora"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatalf("%v\n%s", err, w.Body)
	}

	want := map[string]struct {
		target  string
		drivers bool
	}{
		"windows-11":          {"windows", false},
		"ubuntu-26.04-server": {"ubuntu", true},
		"fedora-44-server":    {"fedora", false},
	}
	seen := map[string]bool{}
	for _, e := range state.Catalog {
		exp, ok := want[e.ID]
		if !ok {
			continue
		}
		seen[e.ID] = true
		if !e.Programs {
			t.Errorf("%s: programs not offered", e.ID)
		}
		if e.AppTarget != exp.target {
			t.Errorf("%s: app_target %q, want %q", e.ID, e.AppTarget, exp.target)
		}
		// Anaconda has no proprietary-driver question, so the dialog must not
		// draw a control for one.
		if e.ThirdPartyDrivers != exp.drivers {
			t.Errorf("%s: third_party_drivers %v, want %v", e.ID, e.ThirdPartyDrivers, exp.drivers)
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("%s is not in the catalog the portal sends", id)
		}
	}

	// And the programs carry both flags, so each list can be filtered without
	// asking the server again. Steam is the useful case: Ubuntu has it, Fedora
	// has no acceptable source for it.
	for _, a := range state.Apps {
		switch a.ID {
		case "steam":
			if !a.Ubuntu || a.Fedora {
				t.Errorf("steam: ubuntu=%v fedora=%v, want true/false", a.Ubuntu, a.Fedora)
			}
		case "vlc":
			if !a.Ubuntu || !a.Fedora {
				t.Errorf("vlc: ubuntu=%v fedora=%v, want both", a.Ubuntu, a.Fedora)
			}
		}
	}
}
