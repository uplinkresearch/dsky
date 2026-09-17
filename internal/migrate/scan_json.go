package migrate

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The readings come back from scan.ps1 as one JSON document, and this is the
// contract between them. Keeping it in its own file, with no Windows in it,
// means the awkward half -- what a machine's answers look like, and what to
// do when a section is missing or a field has the wrong shape -- is tested
// here rather than found out on a customer's PC.
//
// Every section is optional. A machine with a damaged WMI repository answers
// some of these and not others; the scan reports what is missing and keeps
// the rest, so a section that failed is an error from its own method, not a
// refusal to produce a manifest.

type scanJSON struct {
	OS *struct {
		ProductName string `json:"product_name"`
		Build       string `json:"build"`
		Edition     string `json:"edition"`
		Arch        string `json:"arch"`
		Release     string `json:"release"`
	} `json:"os"`
	Hardware *struct {
		Manufacturer string `json:"manufacturer"`
		Model        string `json:"model"`
		Serial       string `json:"serial"`
		CPU          string `json:"cpu"`
		RAMBytes     int64  `json:"ram_bytes"`
		Disks        []Disk `json:"disks"`
	} `json:"hardware"`
	Identity *struct {
		DomainFQDN    string       `json:"domain_fqdn"`
		DomainNetBIOS string       `json:"domain_netbios"`
		OUDN          string       `json:"ou_dn"`
		Groups        []string     `json:"groups"`
		LocalGroups   []LocalGroup `json:"local_groups"`
	} `json:"identity"`
	Apps []struct {
		DisplayName     string `json:"display_name"`
		DisplayVersion  string `json:"display_version"`
		Publisher       string `json:"publisher"`
		InstallLocation string `json:"install_location"`
		RegistryKey     string `json:"registry_key"`
		Arch            string `json:"arch"`
		SourceKind      string `json:"source_kind"`
		SystemComponent bool   `json:"system_component"`
		Framework       bool   `json:"framework"`
		Inbox           bool   `json:"inbox"`
		User            string `json:"user"`
	} `json:"apps"`
	Printers     []Printer        `json:"printers"`
	MappedDrives []MappedDrive    `json:"mapped_drives"`
	Network      *Network         `json:"network"`
	Settings     []rawSettingJSON `json:"settings"`
	Profiles     []struct {
		Account   string `json:"account"`
		Profile   string `json:"profile"`
		LastLogon string `json:"last_logon"`
		SizeBytes int64  `json:"size_bytes"`
		Local     bool   `json:"local"`
	} `json:"profiles"`
	Hints *struct {
		OneDriveKFM       bool     `json:"onedrive_kfm"`
		RedirectedFolders []string `json:"redirected_folders"`
		USMTPath          string   `json:"usmt_path"`
	} `json:"hints"`
	UnsignedDrivers []string `json:"unsigned_drivers"`
	Hostname        string   `json:"hostname"`
	ScannerUser     string   `json:"scanner_user"`
	Problems        []string `json:"problems"`
}

type rawSettingJSON struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
	From  string          `json:"from"`
}

// jsonCollector answers from a finished scan document.
type jsonCollector struct {
	doc  scanJSON
	fail map[string]error // the sections the script said it could not read
}

// ParseScanJSON reads what scan.ps1 wrote. Unknown fields are allowed here,
// unlike the manifest: this is one program's output read by the same program's
// build, and a newer script adding a reading is not a reason for an older
// DSKY to refuse the scan it just ran.
func ParseScanJSON(b []byte) (Collector, error) {
	s := strings.TrimSpace(strings.TrimPrefix(string(b), "\ufeff"))
	if s == "" {
		return nil, fmt.Errorf("the scan produced nothing at all")
	}
	var doc scanJSON
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		// The first line of PowerShell's error is worth more than a JSON
		// parser's offset, so it goes in the message.
		first := s
		if i := strings.IndexAny(first, "\r\n"); i > 0 {
			first = first[:i]
		}
		if len(first) > 200 {
			first = first[:200] + "…"
		}
		return nil, fmt.Errorf("the scan did not produce readable JSON (%w); it said: %s", err, first)
	}
	c := &jsonCollector{doc: doc, fail: map[string]error{}}
	// "identity: The RPC server is unavailable." -> the identity method fails
	// with that, and everything else still answers.
	for _, p := range doc.Problems {
		what, why, ok := strings.Cut(p, ": ")
		if !ok {
			what, why = "the scan", p
		}
		c.fail[what] = fmt.Errorf("%s", why)
	}
	return c, nil
}

func (c *jsonCollector) errFor(section string) error { return c.fail[section] }

func (c *jsonCollector) OS() (SourceOS, error) {
	if err := c.errFor("os"); err != nil {
		return SourceOS{}, err
	}
	if c.doc.OS == nil {
		return SourceOS{}, fmt.Errorf("the scan did not report the Windows version")
	}
	o := *c.doc.OS
	name := strings.TrimSpace(o.ProductName)
	// "Windows 10 Pro" plus "22H2" is what a person would say about the
	// machine, and the release is not otherwise in the manifest.
	if o.Release != "" && !strings.Contains(name, o.Release) {
		name = strings.TrimSpace(name + " " + o.Release)
	}
	return SourceOS{ProductName: name, Build: o.Build, Edition: o.Edition, Arch: o.Arch}, nil
}

func (c *jsonCollector) Hardware() (Hardware, error) {
	if err := c.errFor("hardware"); err != nil {
		return Hardware{}, err
	}
	if c.doc.Hardware == nil {
		return Hardware{}, fmt.Errorf("the scan did not report the hardware")
	}
	h := *c.doc.Hardware
	return Hardware{
		Manufacturer: strings.TrimSpace(h.Manufacturer), Model: strings.TrimSpace(h.Model),
		Serial: strings.TrimSpace(h.Serial), CPU: strings.TrimSpace(h.CPU),
		RAMBytes: h.RAMBytes, Disks: h.Disks,
	}, nil
}

func (c *jsonCollector) Apps(bool) ([]RawApp, error) {
	if err := c.errFor("apps"); err != nil {
		return nil, err
	}
	out := make([]RawApp, 0, len(c.doc.Apps))
	for _, a := range c.doc.Apps {
		out = append(out, RawApp{
			DisplayName: a.DisplayName, DisplayVersion: a.DisplayVersion, Publisher: a.Publisher,
			InstallLocation: a.InstallLocation, RegistryKey: a.RegistryKey, Arch: a.Arch,
			SourceKind: a.SourceKind, SystemComponent: a.SystemComponent,
			Framework: a.Framework, Inbox: a.Inbox, User: a.User,
		})
	}
	return out, nil
}

func (c *jsonCollector) Identity() (RawIdentity, error) {
	if err := c.errFor("identity"); err != nil {
		return RawIdentity{}, err
	}
	if c.doc.Identity == nil {
		return RawIdentity{}, nil // a machine in no domain reports nothing here
	}
	i := *c.doc.Identity
	return RawIdentity{DomainFQDN: i.DomainFQDN, DomainNetBIOS: i.DomainNetBIOS,
		OUDN: i.OUDN, Groups: i.Groups, LocalGroups: i.LocalGroups}, nil
}

func (c *jsonCollector) Printers() ([]Printer, error) {
	if err := c.errFor("printers"); err != nil {
		return nil, err
	}
	// A network queue carries its share path and needs no driver; a queue on
	// an IP port needs the driver named. Both come back from Get-Printer with
	// fields the other does not use, so the empty ones are dropped here.
	out := make([]Printer, 0, len(c.doc.Printers))
	for _, p := range c.doc.Printers {
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" {
			continue
		}
		if p.SharedPath != "" {
			p.Port, p.IP = "", ""
		}
		out = append(out, p)
	}
	return out, nil
}

func (c *jsonCollector) MappedDrives(bool) ([]MappedDrive, error) {
	if err := c.errFor("mapped_drives"); err != nil {
		return nil, err
	}
	out := make([]MappedDrive, 0, len(c.doc.MappedDrives))
	for _, d := range c.doc.MappedDrives {
		if !driveLetterRe.MatchString(d.Letter) || !strings.HasPrefix(d.UNC, `\\`) {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

func (c *jsonCollector) Network() (Network, error) {
	if err := c.errFor("network"); err != nil {
		return Network{}, err
	}
	if c.doc.Network == nil {
		return Network{}, nil
	}
	return *c.doc.Network, nil
}

func (c *jsonCollector) Settings() ([]RawSetting, error) {
	if err := c.errFor("settings"); err != nil {
		return nil, err
	}
	out := make([]RawSetting, 0, len(c.doc.Settings))
	for _, s := range c.doc.Settings {
		var v any
		if err := json.Unmarshal(s.Value, &v); err != nil {
			continue
		}
		// PowerShell divides to get minutes and hands back 30.0; a setting
		// that is a whole number should read as one in the manifest.
		if f, ok := v.(float64); ok && f == float64(int64(f)) {
			v = int64(f)
		}
		out = append(out, RawSetting{Key: s.Key, Value: v, From: s.From})
	}
	return out, nil
}

func (c *jsonCollector) Profiles(bool) ([]UserProf, error) {
	if err := c.errFor("profiles"); err != nil {
		return nil, err
	}
	out := make([]UserProf, 0, len(c.doc.Profiles))
	for _, p := range c.doc.Profiles {
		u := UserProf{Account: p.Account, Profile: p.Profile, SizeBytes: p.SizeBytes, Local: p.Local}
		if p.LastLogon != "" {
			if t, err := time.Parse(time.RFC3339, p.LastLogon); err == nil {
				t = t.UTC()
				u.LastLogon = &t
			}
		}
		out = append(out, u)
	}
	return out, nil
}

func (c *jsonCollector) Hints() (RawHints, error) {
	if err := c.errFor("hints"); err != nil {
		return RawHints{}, err
	}
	if c.doc.Hints == nil {
		return RawHints{}, nil
	}
	h := *c.doc.Hints
	return RawHints{OneDriveKFM: h.OneDriveKFM, RedirectedFolders: h.RedirectedFolders, USMTPath: h.USMTPath}, nil
}

func (c *jsonCollector) UnsignedDrivers() ([]string, error) {
	if err := c.errFor("unsigned_drivers"); err != nil {
		return nil, err
	}
	return c.doc.UnsignedDrivers, nil
}

func (c *jsonCollector) Hostname() (string, error) {
	if h := strings.TrimSpace(c.doc.Hostname); h != "" {
		return h, nil
	}
	return "", fmt.Errorf("the scan did not report the computer name")
}

func (c *jsonCollector) ScannerUser() string { return strings.TrimSpace(c.doc.ScannerUser) }
