package migrate

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ScanToFiles is the whole of what a person does on the machine being
// replaced: run one command, wait a couple of minutes, take the two files
// away. It is here rather than in a command so that the stick's agent and the
// operator's DSKY do exactly the same thing -- the scan that matters is the
// one run from removable media on somebody's desk, and it must not be the
// less-tested path.
//
// Two files are written, and the second is the one that gets read: the
// manifest for DSKY, and the report for the person who has to decide what
// happens to this PC.

// ScanResult says what a scan produced, for the command that ran it.
type ScanResult struct {
	Manifest     *Manifest
	ManifestPath string
	ReportPath   string
	Unread       []error // sections the machine would not answer
}

// ScanToFiles reads this machine and writes manifest.json and report.html
// into dir. An existing manifest is not overwritten in place: a second scan
// of the same machine writes manifest-2.json, because the first may already
// have been reviewed and approved.
func ScanToFiles(ctx context.Context, dir string, opts ScanOptions, usmtPath, version string, progress io.Writer) (*ScanResult, error) {
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	say := func(format string, args ...any) {
		if progress != nil {
			fmt.Fprintf(progress, format+"\n", args...)
		}
	}
	if opts.GeneratedBy == "" {
		opts.GeneratedBy = version
	}
	c, err := NewCollector(ctx, opts, usmtPath)
	if err != nil {
		return nil, err
	}
	say("Reading this computer. Nothing is installed or changed.")
	m, unread := Scan(c, opts)
	res := &ScanResult{Manifest: m, Unread: unread}

	res.ManifestPath = freePath(dir, "manifest", ".json")
	if err := m.Save(res.ManifestPath); err != nil {
		return nil, err
	}
	res.ReportPath = strings.TrimSuffix(res.ManifestPath, ".json") + ".html"
	if base := filepath.Base(res.ManifestPath); base == ManifestName {
		res.ReportPath = filepath.Join(dir, "report.html")
	}
	f, err := os.Create(res.ReportPath)
	if err != nil {
		return nil, err
	}
	if err := Report(f, m, "win11", version); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}

	c2 := m.Count("win11")
	say("")
	say("%s — %s, %s", m.Source.Hostname, machineName(m.Source.Hardware), m.Source.OS.ProductName)
	say("  %d applications, %d settings, %d printers, %d mapped drives", c2.Apps, c2.Settings, c2.Printers, c2.Drives)
	if m.Identity.Domained() {
		say("  in %s", m.Identity.DomainFQDN)
	}
	say("  user files: %s", dataInWords(m.Data))
	if c2.Blockers > 0 || c2.Warnings > 0 {
		say("  %d blocker(s), %d warning(s) — see the report", c2.Blockers, c2.Warnings)
	}
	for _, err := range unread {
		say("  could not read %v", err)
	}
	say("")
	say("Wrote %s", res.ManifestPath)
	say("      %s", res.ReportPath)
	say("Next: read the report, then on your own machine run")
	say("      dsky migrate resolve %s", filepath.Base(res.ManifestPath))
	return res, nil
}

func dataInWords(d Data) string {
	switch d.Strategy {
	case DataUSMT:
		return fmt.Sprintf("on this disk — %d profile(s) to copy with USMT", len(d.Users))
	case DataKFM:
		return "in OneDrive; they come back at first sign-in"
	case DataRedirect:
		return "on the file server through folder redirection"
	default:
		return "nothing to carry over"
	}
}

// freePath keeps an earlier scan of the same machine: it may already have
// been reviewed, and overwriting a reviewed plan with a fresh one would throw
// away the decisions in it without saying so.
func freePath(dir, stem, ext string) string {
	path := filepath.Join(dir, stem+ext)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}
	for i := 2; ; i++ {
		path = filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, i, ext))
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return path
		}
	}
}
