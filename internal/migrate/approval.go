package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// The review step is the point of this file. A migration builds a machine
// from a plan a person read, and "a person read it" has to be checkable later
// -- by the builder, and by whoever asks afterwards why some application is on
// a new PC. So approval is a hash of the plan, not a flag: approve records the
// hash of everything except the approval itself, and any later edit, by any
// tool or hand, no longer matches.
//
// The hash is over the compact JSON of the manifest with the approval section
// zeroed. Go writes struct fields in declaration order and sorts map keys, so
// the same manifest hashes the same on every machine and every build; the two
// raw-JSON fields (a setting's value, the pass-through DSKY options) keep the
// bytes the scanner wrote, so re-saving a manifest does not change its hash.

// Hash is the content hash of the manifest: SHA-256 over every section except
// approval, hex encoded.
func (m *Manifest) Hash() (string, error) {
	copyOf := *m
	copyOf.Approval = Approval{}
	b, err := json.Marshal(&copyOf)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Approve records that somebody read this plan and accepted it. It refuses
// what the review itself must refuse, so that no other path can approve a
// manifest with work left in it: an application nobody placed, or a blocker
// nobody answered.
func (m *Manifest) Approve(by string) error {
	if by == "" {
		return fmt.Errorf("approving a manifest needs a name to record")
	}
	if err := m.ReadyToApprove(); err != nil {
		return err
	}
	h, err := m.Hash()
	if err != nil {
		return err
	}
	now := time.Now().UTC().Truncate(time.Second)
	m.Approval = Approval{Approved: true, ApprovedAt: &now, ApprovedBy: by, ContentHash: h}
	return nil
}

// ReadyToApprove says what is still in the way, in one sentence a person can
// act on, or nil when nothing is.
func (m *Manifest) ReadyToApprove() error {
	c := m.Count("win11")
	if c.UnackedBlockers > 0 {
		for _, x := range m.Compat {
			if x.Severity == Blocker && !x.Acknowledged {
				return fmt.Errorf("%d blocker(s) not answered, starting with %s: %s — accept the risk or drop it",
					c.UnackedBlockers, m.subjectName(x.Subject), x.Reason)
			}
		}
	}
	if m.Data.Strategy == DataUSMT {
		switch {
		case m.Data.StorePath == "":
			return fmt.Errorf("the files are to be copied with USMT and no store has been named — give data.store_path a share, or choose another way to move them")
		case len(m.Data.Users) == 0:
			return fmt.Errorf("the files are to be copied with USMT and nobody is named — say whose files in data.users, or choose another way to move them")
		}
	}
	if c.Unmapped > 0 {
		for _, a := range m.Apps {
			if s := a.Resolution.Status; s == StatusUnmapped || s == StatusUnset {
				return fmt.Errorf("%d application(s) have nowhere to install from, starting with %q — give it a source, mark it manual, or drop it",
					c.Unmapped, a.DisplayName)
			}
		}
	}
	return nil
}

// subjectName is what a person calls the thing a compat entry is about. The
// entries carry an application id, which is DSKY's word for it; everybody
// else knows it by the name in Settings -> Apps.
func (m *Manifest) subjectName(subject string) string {
	for _, a := range m.Apps {
		if a.ID == subject && a.DisplayName != "" {
			return a.DisplayName
		}
	}
	return subject
}

// Unapprove clears the approval, for a manifest being reopened. Editing
// without this leaves a stale hash, which the builder reports as an edited
// plan -- true, but less clear than a plan that says it is not approved.
func (m *Manifest) Unapprove() { m.Approval = Approval{} }

// CheckApproved is what the builder calls: approved by somebody, and
// unchanged since. Both failures are refusals to build, and each says which
// one it is, because they are fixed differently -- one needs a review, the
// other needs the review done again.
func (m *Manifest) CheckApproved() error {
	if !m.Approval.Approved {
		return fmt.Errorf("this manifest is not approved — review it first (dsky migrate review)")
	}
	h, err := m.Hash()
	if err != nil {
		return err
	}
	if h != m.Approval.ContentHash {
		return fmt.Errorf("this manifest was edited after %s approved it — review it again (dsky migrate review)", m.Approval.ApprovedBy)
	}
	return nil
}
