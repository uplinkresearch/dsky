package cli

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/uplinkresearch/dsky/internal/library"
)

// A portal started with --new is an extra, not a replacement, and must leave
// the record alone.
//
// The record is how the next launch finds the one portal to raise. When a
// second instance took it, the app handed its window to whatever had been
// started alongside -- a development build in a terminal, typically -- which
// then answered for the installed copy. Its Update button replaced that
// build's binary instead of the one on disk, and it had no window to restart,
// so an update reported success, changed nothing, and never came back.
func TestASecondPortalDoesNotTakeTheRecord(t *testing.T) {
	root := t.TempDir()
	lib, err := library.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	read := func() instance {
		t.Helper()
		b, err := os.ReadFile(instancePath(root))
		if err != nil {
			t.Fatal("no portal record:", err)
		}
		var inst instance
		if err := json.Unmarshal(b, &inst); err != nil {
			t.Fatal(err)
		}
		return inst
	}

	if _, _, err := startServer(ctx, lib, nil, 8973, ".", false, false, true, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	first := read()
	if first.Port == 0 || first.Token == "" {
		t.Fatalf("the first portal recorded nothing usable: %+v", first)
	}

	if _, _, err := startServer(ctx, lib, nil, 8974, ".", false, false, false, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	if now := read(); now.Port != first.Port || now.Token != first.Token {
		t.Errorf("a --new portal took the record: port %d token %s, want port %d token %s",
			now.Port, now.Token, first.Port, first.Token)
	}
}
