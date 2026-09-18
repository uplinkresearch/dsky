package cli

import (
	"context"
	"testing"
	"time"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
	"github.com/uplinkresearch/dsky/internal/library"
)

// Every portal must load the operator's own installers, whichever way it was
// started. The app people click does not go through the command path, so
// installers added in its window were written to disk and then invisible the
// next time it opened -- gone from "Your installers", and missing from every
// recipe that used one.
func TestPortalLoadsTheOperatorsInstallers(t *testing.T) {
	root := t.TempDir()
	added := appcatalog.Custom{
		ID: "macula", Name: "Macula Agent", Format: "msi",
		SHA256:   "ab" + "cd" + "ef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		Filename: "macula-agent.msi", Size: 1234,
	}
	if err := appcatalog.AddCustom(root, added, false); err != nil {
		t.Fatal(err)
	}
	// Forget it, the way a fresh process has never heard of it.
	if err := appcatalog.LoadCustom(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if len(appcatalog.CustomApps()) != 0 {
		t.Fatalf("the list did not start empty: %v", appcatalog.CustomApps())
	}

	lib, err := library.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, _, err := startServer(ctx, lib, nil, 8971, ".", false, false, true, time.Minute, nil); err != nil {
		t.Fatal(err)
	}

	got := appcatalog.CustomApps()
	if len(got) != 1 || got[0].ID != "macula" {
		t.Errorf("the portal started without the operator's installers: %v", got)
	}
	if _, ok := appcatalog.Get("macula"); !ok {
		t.Error("the program pickers would not offer it")
	}
}
