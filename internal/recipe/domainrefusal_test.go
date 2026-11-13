package recipe

import (
	"strings"
	"testing"
)

// DSKY no longer joins a PC to a domain, and a recipe that still asks for one
// is refused by name rather than ignored. A recipe that quietly lost its join
// would hand back workgroup machines that look perfectly finished.
func TestARecipeThatJoinsADomainIsRefused(t *testing.T) {
	for _, domain := range []any{
		map[string]any{"blob": "pc.txt"},
		map[string]any{"join": "corp.example.com", "username": "svc", "password": "${var:pw}"},
		map[string]any{"blobs_by_serial": "join-files"},
	} {
		r := appsRecipe("machine", "Google.Chrome")
		r.Windows.Domain = domain
		err := r.Validate()
		if err == nil {
			t.Fatalf("a recipe with windows.domain %v was accepted", domain)
		}
		for _, want := range []string{"windows.domain", "removed", "by hand"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal never mentions %q:\n%v", want, err)
			}
		}
	}
	if err := appsRecipe("machine", "Google.Chrome").Validate(); err != nil {
		t.Errorf("a recipe with no domain was refused: %v", err)
	}
}
