package appcatalog

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/uplinkresearch/dsky/internal/fetch"
	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/manifest"
)

// Pulling the catalog's programs onto the media at build time.
//
// The ordinary way a program arrives is winget at first boot: nothing large
// rides on the stick, every installer comes from the vendor, and the machine
// needs the internet for ten minutes. On a lot of jobs it does not have it.
// A new site's network is the thing being installed; a bench has no wifi
// password to hand; the van has neither. Those machines came out of setup with
// no programs at all and a technician doing it again by hand later.
//
// Offline builds fetch the same installers here, on the machine doing the
// building, and put them on the stick. Three properties are load-bearing:
//
//   - The file is the one winget's manifest names, checked against the hash in
//     that manifest. Not the vendor's "latest" link. An installer that arrives
//     over a coffee-shop connection and is never checked is not something to
//     put on every machine in a fleet.
//   - The installer is the real one, so Add/Remove Programs gets the entry the
//     vendor writes, and `winget upgrade` can match the package afterwards.
//     Something equivalent-but-different would leave every machine holding a
//     program winget does not recognise and will never update.
//   - The version is pinned to the day the stick was built, and the stick
//     records it, because that is the honest part: offline means frozen. See
//     the catch-up in the agent, which is how the freeze ends.
//
// A package that cannot be fetched this way stops the build by name. Silently
// leaving one out would produce a stick that looks complete and is not.

// Prepulled is one catalog program whose installer now rides on the media.
type Prepulled struct {
	WingetID string   // Google.Chrome — what the catch-up upgrades later
	Version  string   // as the manifest spelled it on the day of the build
	SHA256   string   // the library blob, which is also the manifest's pin
	Filename string   // what it is called on the stick
	Format   string   // msi | exe
	Args     []string // silent switches; empty for an MSI (msiexec's own)
	Size     int64
}

// SourceID is the library/manifest id the installer is filed under. The
// winget- prefix keeps it clear of the app- ids operator installers use, so a
// pre-pulled Chrome and an operator's own Chrome MSI can sit in one library.
//
// A winget id may contain a plus sign and a manifest id may not:
// Microsoft.VCRedist.2015+.x64 is real, is what LibreOffice and KeePassXC and
// Plex all depend on, and failed the build outright -- found by building a
// stick rather than by reading the two regular expressions, which say
// different things a screen apart in different packages. Anything outside
// what a manifest id allows becomes a dash; the winget id itself is kept in
// full in the record, which is what the machine upgrades by.
func (p Prepulled) SourceID() string {
	return "winget-" + strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		}
		return '-'
	}, strings.ToLower(p.WingetID))
}

// MSI reports whether msiexec runs this file.
func (p Prepulled) MSI() bool { return p.Format == "msi" }

// puller is the slice of the library this needs, kept narrow so tests need no
// real library.
type puller interface {
	Pull(ctx context.Context, src *manifest.Source, pinTOFU bool, resolve library.URLResolver,
		progress fetch.Progress, note func(string)) (library.Entry, error)
}

// PrePullError names every program that cannot ride on the media, and why.
// One error rather than the first one, because an operator who has ticked
// offline wants the whole list before they decide what to do about it, not
// one package per build attempt.
type PrePullError struct {
	Reasons []string
}

func (e *PrePullError) Error() string {
	return "these programs cannot be put on the media for an offline install:\n  - " +
		strings.Join(e.Reasons, "\n  - ")
}

// Resolvable reports whether err is the build refusing a package rather than
// the lookup itself failing, which callers word differently: one is "pick
// different programs", the other is "try again when the network is back".
func Resolvable(err error) bool {
	return errors.Is(err, ErrNoOfflineInstaller) || errors.Is(err, ErrWingetNotFound)
}

// maxDependencyDepth bounds how far a chain is followed. One level covers
// every chain the built-in list has -- LibreOffice wants a Visual C++
// runtime, HandBrake wants a .NET desktop runtime -- and two leaves room for
// a runtime that itself names one. Beyond that the build refuses and names
// the chain, rather than quietly downloading a tree of packages nobody asked
// for onto somebody's stick.
const maxDependencyDepth = 2

// PrePull looks up and downloads the installer for each winget id, returning
// them in the order they must be installed: a package's dependencies come
// before it, because on a machine with no network there is nothing else to
// fetch the Visual C++ runtime LibreOffice will not start without.
//
// Every manifest is read before anything is downloaded. Reading them costs a
// few seconds; downloading them costs a few hundred megabytes, and an
// operator who has picked one Store-only program should not pay that before
// being told the build cannot be made.
//
// progress and note are the library's, so the caller can show which file is
// coming down; either may be nil.
func PrePull(ctx context.Context, lib puller, ids []string,
	progress fetch.Progress, note func(string)) ([]Prepulled, error) {

	plan, err := planPrePull(ctx, ids)
	if err != nil {
		return nil, err
	}
	var out []Prepulled
	for _, in := range plan {
		p := Prepulled{
			WingetID: in.ID,
			Version:  in.Version,
			SHA256:   in.SHA256,
			Filename: in.Filename(),
			Format:   "exe",
			Args:     in.Args,
		}
		if in.MSI() {
			p.Format = "msi"
		}
		if note != nil {
			note(fmt.Sprintf("%s %s", in.ID, in.Version))
		}
		entry, err := lib.Pull(ctx, &manifest.Source{
			ID:       p.SourceID(),
			Kind:     manifest.KindPayload,
			Format:   manifest.Format(p.Format),
			URL:      in.URL,
			SHA256:   in.SHA256,
			Filename: p.Filename,
		}, false, nil, progress, note)
		if err != nil {
			return nil, fmt.Errorf("downloading %s %s: %w", in.ID, in.Version, err)
		}
		p.Size = entry.Size
		out = append(out, p)
	}
	return out, nil
}

// planPrePull reads the manifests and works out what has to go on the media,
// in install order, or refuses with every reason it found. Nothing is
// downloaded here.
func planPrePull(ctx context.Context, ids []string) ([]WingetInstaller, error) {
	var plan []WingetInstaller
	var reasons []string
	seen := map[string]bool{}

	// Depth-first so a dependency is planned, and installed, before the
	// package that named it.
	var add func(id string, depth int, chain []string) error
	add = func(id string, depth int, chain []string) error {
		if seen[strings.ToLower(id)] {
			return nil
		}
		in, err := LookupWingetInstaller(ctx, id)
		if err != nil {
			if !Resolvable(err) {
				// Offline, or GitHub's hourly limit: nothing is known about
				// this package either way, so the build stops here rather
				// than reporting a package as impossible when it was never
				// asked about.
				return fmt.Errorf("%s: %w", id, err)
			}
			seen[strings.ToLower(id)] = true
			reasons = append(reasons, trimReason(id, err, chain))
			return nil
		}
		if len(in.Needs) > 0 {
			if depth >= maxDependencyDepth {
				seen[strings.ToLower(in.ID)] = true
				reasons = append(reasons, fmt.Sprintf("%s — it needs %s, which needs more again; DSKY stops following a chain after %d",
					describe(id, chain), strings.Join(in.Needs, ", "), maxDependencyDepth))
				return nil
			}
			for _, need := range in.Needs {
				if err := add(need, depth+1, append(chain, in.ID)); err != nil {
					return err
				}
			}
		}
		// Marked under the manifest's own spelling as well as what was asked
		// for, so two packages naming the same runtime differently do not
		// carry it twice.
		seen[strings.ToLower(id)] = true
		seen[strings.ToLower(in.ID)] = true
		plan = append(plan, in)
		return nil
	}

	for _, id := range ids {
		if err := add(id, 0, nil); err != nil {
			return nil, err
		}
	}
	if len(reasons) > 0 {
		return nil, &PrePullError{Reasons: reasons}
	}
	return plan, nil
}

// describe names a package, and says what wanted it when it is not one the
// operator picked: "the Visual C++ runtime cannot be downloaded" is a puzzle
// on its own, and obvious as soon as LibreOffice is named beside it.
func describe(id string, chain []string) string {
	if len(chain) == 0 {
		return id
	}
	return fmt.Sprintf("%s (needed by %s)", id, strings.Join(chain, " → "))
}

// trimReason turns one lookup refusal into a line for the list. The sentinel's
// own words ("this package cannot be downloaded ahead of time: ") are dropped:
// the list's heading already says that, and repeating it once per line makes
// the part that differs hard to find.
func trimReason(id string, err error, chain []string) string {
	name := describe(id, chain)
	if errors.Is(err, ErrWingetNotFound) {
		return name + " — winget has no package with that id"
	}
	msg := err.Error()
	if i := strings.Index(msg, ErrNoOfflineInstaller.Error()+": "); i >= 0 {
		msg = msg[i+len(ErrNoOfflineInstaller.Error())+2:]
	}
	// pickInstaller's message already opens with the package id; where it
	// does, the chain replaces it so the line reads as one sentence.
	if rest, ok := strings.CutPrefix(msg, id+" — "); ok {
		return name + " — " + rest
	}
	return name + " — " + msg
}
