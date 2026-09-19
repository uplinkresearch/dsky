// Package uninstall removes what the installer put on a machine.
//
// It works as a plan you can look at before anything happens, because the
// footprint spans three very different kinds of thing: a few megabytes of
// program, a little configuration, and a library that routinely holds tens
// of gigabytes of downloaded operating systems. Deleting the first two is
// obviously right; throwing away the third without asking would discard
// hours of downloading, so the library is opt-in and always priced in the
// plan.
//
// Workspaces are never touched and never listed. They are the operator's own
// git repositories — recipes, manifests, templates, org configuration — and
// they live wherever that person keeps their code. An uninstaller that
// deleted one would be destroying work it did not create.
package uninstall

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/uplinkresearch/dsky/internal/appconfig"
	"github.com/uplinkresearch/dsky/internal/library"
)

// Kind groups an item so callers can explain and gate it.
type Kind string

const (
	KindProgram  Kind = "program"  // binaries, shortcuts, launchers
	KindSettings Kind = "settings" // per-user configuration
	KindLibrary  Kind = "library"  // downloaded images and built artifacts
	KindPath     Kind = "path"     // a PATH entry rather than a file
)

// Item is one thing to remove.
type Item struct {
	Path  string
	What  string // human description
	Kind  Kind
	Bytes int64 // 0 when unknown or not a file
	// Self marks the running executable, which on Windows cannot delete
	// itself and needs handing to a detached helper.
	Self bool
}

// Plan is everything an uninstall would remove.
type Plan struct {
	Items []Item
	// Missing records footprint that was already absent, so the report can
	// say "not installed" rather than silently listing nothing.
	Missing []string
}

// Bytes totals the plan, optionally including the library.
func (p Plan) Bytes(withLibrary bool) int64 {
	var n int64
	for _, it := range p.Items {
		if it.Kind == KindLibrary && !withLibrary {
			continue
		}
		n += it.Bytes
	}
	return n
}

// LibraryBytes is what the cached images and artifacts occupy.
func (p Plan) LibraryBytes() int64 {
	var n int64
	for _, it := range p.Items {
		if it.Kind == KindLibrary {
			n += it.Bytes
		}
	}
	return n
}

// Build works out what is present on this machine.
func Build() (*Plan, error) {
	p := &Plan{}
	self, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}

	for _, it := range programItems(self) {
		p.add(it)
	}
	p.add(Item{Path: appconfig.Dir(), What: "settings (recent workspaces)", Kind: KindSettings})
	p.add(Item{Path: library.DefaultRoot(), What: "library — downloaded OS images, drivers, built media", Kind: KindLibrary})
	return p, nil
}

// add keeps present paths and notes absent ones. PATH entries are not files,
// so they are added as-is.
func (p *Plan) add(it Item) {
	if it.Kind == KindPath {
		p.Items = append(p.Items, it)
		return
	}
	st, err := os.Stat(it.Path)
	if err != nil {
		p.Missing = append(p.Missing, it.Path)
		return
	}
	if st.IsDir() {
		it.Bytes = dirBytes(it.Path)
	} else {
		it.Bytes = st.Size()
	}
	p.Items = append(p.Items, it)
}

func dirBytes(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// Run removes the plan. The library is removed only when withLibrary is set.
// Removal continues past failures so one locked file cannot strand the rest;
// every failure is returned.
func Run(p *Plan, withLibrary bool) (removed []string, errs []error) {
	// Plural. The alias is a symlink to the program, and os.Executable()
	// resolves through it, so running either one marks *both* items as self.
	// A single pointer kept only the last of them: `dsky uninstall` deferred
	// dsky, then overwrote that with compose, removed compose at the end, and
	// left the program on the machine -- reporting "Removed 5 item(s)" of six
	// listed, with no error, because nothing had tried and failed. The count
	// was the only clue, and counting is not something anybody does to a
	// success message.
	var selves []*Item
	for i := range p.Items {
		it := p.Items[i]
		if it.Kind == KindLibrary && !withLibrary {
			continue
		}
		if it.Self {
			selves = append(selves, &p.Items[i]) // last, so a failure strands nothing
			continue
		}
		switch it.Kind {
		case KindPath:
			if err := removeFromPath(it.Path); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", it.What, err))
				continue
			}
		default:
			if err := os.RemoveAll(it.Path); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", it.Path, err))
				continue
			}
		}
		removed = append(removed, it.Path)
	}
	for _, self := range selves {
		if err := removeSelf(self.Path); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", self.Path, err))
			continue
		}
		removed = append(removed, self.Path)
	}
	return removed, errs
}

// Installed reports whether this looks like an installed copy rather than one
// run out of a build directory, so the report can say so.
func Installed(p *Plan) bool {
	for _, it := range p.Items {
		if it.Kind == KindProgram {
			return true
		}
	}
	return false
}

// sameDir reports whether path sits inside dir.
func sameDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && !strings.HasPrefix(rel, "..")
}
