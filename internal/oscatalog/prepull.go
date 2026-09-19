package oscatalog

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
	"github.com/uplinkresearch/dsky/internal/library"
)

// Building media that installs programs with no internet.
//
// The ordinary build puts a list of winget ids on the stick and the machine
// fetches them from the vendors at first boot. An offline build fetches them
// here instead, on the computer doing the building, and carries the installers.
// Everything downstream of that is machinery that already existed: a
// pre-pulled Chrome is staged, manifested, refused for size and run at first
// boot by exactly the same code as an MSP's own RMM installer. That is the
// point of doing it this way rather than teaching the agent a second kind of
// program -- there is only one path onto the media, and it is the one that has
// been carrying somebody's agent installer to a bench for a year.
//
// What is new is the record: which winget package each of those files is, and
// what version it was on the day of the build. Without it an offline machine
// is a machine with a pile of programs nothing can account for. See
// internal/agent/catchup.go for what the machine does with it.

// fat32MaxFile is the largest file FAT32 holds, and Windows media is FAT32 so
// that it boots on machines that will not read anything else. An installer
// bigger than this cannot go on the stick at all, and finding that out at the
// end of a gigabyte download is worse than finding it out now.
const fat32MaxFile = 4*1024*1024*1024 - 1

// prePull downloads the installers for this build's catalog programs and
// records them on opts. It is a no-op unless the build asked to be offline.
//
// It runs before the OS image is fetched, because it is the step most likely
// to refuse: an operator who picked a Store-only program should learn that in
// the first few seconds, not after five gigabytes of Windows.
func prePull(ctx context.Context, lib *library.Library, e Entry, opts *Options,
	progress func(stage string, done, total int64)) error {

	if !opts.Offline || e.Family != Windows {
		return nil
	}
	pkgs, _ := opts.resolvedApps()
	if len(pkgs) == 0 {
		// Nothing from the catalog: an operator's own installers already ride
		// on the media, so this build is offline without anything to do.
		opts.builtAt = time.Now().UTC().Format(time.RFC3339)
		return nil
	}
	var what string
	pulled, err := appcatalog.PrePull(ctx, lib, pkgs, func(done, total int64) {
		if progress != nil {
			progress("downloading "+what, done, total)
		}
	}, func(s string) {
		what = s
		if progress != nil {
			progress("downloading "+s, 0, -1)
		}
	})
	if err != nil {
		return err
	}
	for _, p := range pulled {
		if p.Size > fat32MaxFile {
			return fmt.Errorf("%s's installer is %s, and Windows media is FAT32, which holds no file over 4 GB. "+
				"Leave it out of the offline build and install it on the machine afterwards",
				p.WingetID, humanSize(p.Size))
		}
	}
	opts.prepulled = pulled
	opts.builtAt = time.Now().UTC().Format(time.RFC3339)
	return nil
}

// humanSize is for one error message, so it stops at gigabytes.
func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d bytes", n)
}

// prepulledPrefix is how a pre-pulled installer's manifest id starts, and how
// one is told apart from an operator's own (app-) or a driver pack's.
const prepulledPrefix = "winget-"

// prepulledIDs are the manifest ids this build needs, for pruning the rest.
func prepulledIDs(opts Options) map[string]bool {
	out := map[string]bool{}
	for _, a := range opts.prepulled {
		out[a.SourceID()] = true
	}
	return out
}

// prepulledManifestYAML pins a pre-pulled installer to the library blob it was
// downloaded into. No url: the download already happened and was checked
// against winget's own hash, so nothing fetches it again at build time.
func prepulledManifestYAML(p appcatalog.Prepulled) string {
	return fmt.Sprintf("id: %s\nkind: payload\nformat: %s\nsha256: %q\nfilename: %s\n",
		p.SourceID(), p.Format, p.SHA256, p.Filename)
}

// offlineAppsYAML is the payload block, the run steps and the record for the
// pre-pulled programs. The three come back together because they have to
// agree: a run step for a ref with no payload entry fails the build, and a
// record naming a package with no run step would have a machine trying to
// update something it never installed.
func offlineAppsYAML(opts Options) (payload, steps, record string) {
	if len(opts.prepulled) == 0 {
		return "", "", ""
	}
	var p, s, r strings.Builder
	for _, a := range opts.prepulled {
		fmt.Fprintf(&p, "    - { ref: %s }\n", a.SourceID())
		verb := "exe"
		if a.MSI() {
			verb = "msi"
		}
		if args := a.Args; len(args) > 0 {
			quoted := make([]string, len(args))
			for i, v := range args {
				quoted[i] = fmt.Sprintf("%q", v)
			}
			fmt.Fprintf(&s, "      - %s: { ref: %s, args: [%s] }\n", verb, a.SourceID(), strings.Join(quoted, ", "))
		} else {
			fmt.Fprintf(&s, "      - %s: { ref: %s }\n", verb, a.SourceID())
		}
		fmt.Fprintf(&r, "      - { id: %s, version: %q, ref: %s }\n", a.WingetID, a.Version, a.SourceID())
	}
	return p.String(), s.String(), r.String()
}

// writePrepulledManifests puts one manifest beside each downloaded installer
// so the recipe's payload refs resolve to the library blobs.
func writePrepulledManifests(opts Options, write func(rel string, data []byte) error) error {
	for _, a := range opts.prepulled {
		if err := write(filepath.ToSlash(filepath.Join("manifests", a.SourceID()+".yaml")),
			[]byte(prepulledManifestYAML(a))); err != nil {
			return err
		}
	}
	return nil
}
