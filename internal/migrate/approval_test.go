package migrate

import (
	"path/filepath"
	"strings"
	"testing"
)

// The review step is the safety rail of this whole feature: a machine is
// rebuilt from a plan a person read. These tests are about the two ways that
// can go wrong -- approving a plan that still has work in it, and building a
// plan that was edited after somebody approved it.

func TestApprovingRefusesUnfinishedWork(t *testing.T) {
	m := load(t, "customized")
	err := m.Approve("dusty")
	if err == nil {
		t.Fatal("a manifest with a blocker and an unmapped application was approved")
	}
	if !strings.Contains(err.Error(), "blocker") {
		t.Errorf("error should lead with the blocker: %v", err)
	}
	// Answering the blocker leaves the unmapped application, and says so.
	for i := range m.Compat {
		m.Compat[i].Acknowledged = true
	}
	err = m.Approve("dusty")
	if err == nil || !strings.Contains(err.Error(), "nowhere to install from") {
		t.Fatalf("after the blocker, the unmapped application should stop approval: %v", err)
	}
	// The operator settles the last two: one gets an installer, one is dropped.
	for i, a := range m.Apps {
		switch a.ID {
		case "fieldmapssync":
			m.Apps[i].Resolution = Resolution{Status: StatusResolved, Method: MethodEXE,
				Ref: `\\wwfo-fs01\installers\fieldsync\setup.exe`, Args: []string{"/S"},
				ResolvedBy: ByOperator, Confidence: 1, Order: OrderDefault}
		case "oldcadviewer":
			m.Apps[i].Resolution = Resolution{Status: StatusDropped, ResolvedBy: ByOperator}
		}
	}
	if err := m.Approve("dusty"); err != nil {
		t.Fatalf("a settled manifest was still refused: %v", err)
	}
	if !m.Approval.Approved || m.Approval.ApprovedBy != "dusty" || m.Approval.ApprovedAt == nil {
		t.Errorf("approval: %+v", m.Approval)
	}
	if err := m.CheckApproved(); err != nil {
		t.Errorf("freshly approved manifest refused: %v", err)
	}
	if err := m.Approve(""); err == nil {
		t.Error("approved with no name")
	}
}

// The hash covers the plan and not the approval, so approving does not
// invalidate itself, saving and loading keeps it valid, and any edit anywhere
// else breaks it.
func TestTheHashCatchesEveryEdit(t *testing.T) {
	m := load(t, "office")
	if err := m.Approve("dusty"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.CheckApproved(); err != nil {
		t.Fatalf("approval did not survive the file: %v", err)
	}

	edits := map[string]func(*Manifest){
		"an application added":    func(m *Manifest) { m.Apps = append(m.Apps, App{ID: "x", DisplayName: "X", SourceKind: SourceWin32}) },
		"an install source moved": func(m *Manifest) { m.Apps[1].Resolution.Ref = "Evil.Payload" },
		"a different OU":          func(m *Manifest) { m.Identity.ComputerOUDN = "OU=Domain Controllers,DC=mead,DC=local" },
		"another hostname":        func(m *Manifest) { m.Target.Hostname = "MEAD-FRONT-03" },
		"a data strategy change":  func(m *Manifest) { m.Data = Data{Strategy: DataNoneStrat} },
		"a note added":            func(m *Manifest) { m.Notes = append(m.Notes, "trust me") },
		"a setting value change":  func(m *Manifest) { m.Settings[1].Value = []byte("120") },
	}
	for what, edit := range edits {
		edited, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		edit(edited)
		err = edited.CheckApproved()
		if err == nil {
			t.Errorf("%s: still counted as approved", what)
			continue
		}
		if !strings.Contains(err.Error(), "edited after") {
			t.Errorf("%s: %v", what, err)
		}
	}
}

func TestAnUnapprovedManifestIsNotBuildable(t *testing.T) {
	m := load(t, "office")
	err := m.CheckApproved()
	if err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Fatalf("an unapproved manifest passed: %v", err)
	}
	if err := m.Approve("dusty"); err != nil {
		t.Fatal(err)
	}
	m.Unapprove()
	if err := m.CheckApproved(); err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Errorf("reopened manifest: %v", err)
	}
	// Reopening leaves a manifest that still validates; an operator has to be
	// able to save one mid-review.
	if err := m.Validate(); err != nil {
		t.Errorf("reopened manifest no longer valid: %v", err)
	}
}

// Two machines with the same plan must not collide, and the same manifest must
// hash the same on every machine that reads it -- the builder recomputes it.
func TestHashIsStableAndSpecific(t *testing.T) {
	a := load(t, "office")
	b := load(t, "office")
	ha, err := a.Hash()
	if err != nil {
		t.Fatal(err)
	}
	hb, _ := b.Hash()
	if ha != hb {
		t.Errorf("same manifest hashed differently:\n%s\n%s", ha, hb)
	}
	c := load(t, "kiosk")
	hc, _ := c.Hash()
	if ha == hc {
		t.Error("two different manifests share a hash")
	}
	if len(ha) != 64 {
		t.Errorf("hash %q is not a sha256", ha)
	}
}
