package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/uplinkresearch/dsky/internal/fetch"
	"github.com/uplinkresearch/dsky/internal/helpers"
)

// Alienware is in none of Dell's driver-pack catalogs: DriverPackCatalog.cab
// covers Latitude, OptiPlex, Precision and XPS, and CatalogPC.cab the same
// commercial lines. What does cover it is Dell's consumer update index,
// CatalogIndexPC.cab, which names a catalog per system ID -- and each of those
// lists the model's Dell Update Packages one by one, with SHA-256, release
// date and the PCI devices each is for.
//
// So there is no Alienware driver pack. A model's drivers are the newest
// package for each device, taken from its catalog: driver packages only, since
// the same catalogs carry BIOS, firmware and Alienware Command Center, none of
// which belong on an install stick. Each package is a Dell Update Package,
// which unpacks with the same /s /e= switches as Dell's driver packs, and the
// INFs it unpacks are swept by pnputil like any other pack.
const dellIndexPCURL = "https://downloads.dell.com/catalog/CatalogIndexPC.cab"

type alienwareFeed struct{ cache *Cache }

func (f *alienwareFeed) Vendor() Vendor { return Alienware }

// alienwareBrands are the index's brand prefixes for Alienware: notebooks and
// desktops. Every Alienware system sits under one of the two.
var alienwareBrands = map[string]bool{"ANWNB": true, "ANWDT": true}

type dellIndex struct {
	Groups []struct {
		Brands []struct {
			Prefix string `xml:"prefix,attr"`
			Models []struct {
				SystemID string `xml:"systemID,attr"`
				Display  string `xml:"Display"`
			} `xml:"Model"`
		} `xml:"SupportedSystems>Brand"`
		Manifest struct {
			Path   string `xml:"path,attr"`
			Size   int64  `xml:"size,attr"`
			Hashes []struct {
				Algorithm string `xml:"algorithm,attr"`
				Value     string `xml:",chardata"`
			} `xml:"Cryptography>Hash"`
		} `xml:"ManifestInformation"`
	} `xml:"GroupManifest"`
}

// alienwareSystem is one system ID's catalog, as the index describes it.
type alienwareSystem struct {
	Model    string
	SystemID string
	Path     string
	SHA256   string
}

type dellGroupCatalog struct {
	Components []struct {
		Path        string `xml:"path,attr"`
		Size        int64  `xml:"size,attr"`
		DateTime    string `xml:"dateTime,attr"`
		ReleaseDate string `xml:"releaseDate,attr"`
		Vendor      string `xml:"vendorVersion,attr"`
		DellVersion string `xml:"dellVersion,attr"`
		Name        string `xml:"Name>Display"`
		Type        struct {
			Value string `xml:"value,attr"`
		} `xml:"ComponentType"`
		Category struct {
			Value string `xml:"value,attr"`
		} `xml:"Category"`
		OS []struct {
			Code string `xml:"osCode,attr"`
			Arch string `xml:"osArch,attr"`
		} `xml:"SupportedOperatingSystems>OperatingSystem"`
		Devices []struct {
			ComponentID string `xml:"componentID,attr"`
			PCI         []struct {
				Vendor    string `xml:"vendorID,attr"`
				Device    string `xml:"deviceID,attr"`
				SubVendor string `xml:"subVendorID,attr"`
				SubDevice string `xml:"subDeviceID,attr"`
			} `xml:"PCIInfo"`
		} `xml:"SupportedDevices>Device"`
		DCHDevices []struct {
			ComponentID string `xml:"componentID,attr"`
			PCI         []struct {
				Vendor    string `xml:"vendorID,attr"`
				Device    string `xml:"deviceID,attr"`
				SubVendor string `xml:"subVendorID,attr"`
				SubDevice string `xml:"subDeviceID,attr"`
			} `xml:"PCIInfo"`
		} `xml:"SupportedDCHDevices>Device"`
		Hashes []struct {
			Algorithm string `xml:"algorithm,attr"`
			Value     string `xml:",chardata"`
		} `xml:"Cryptography>Hash"`
	} `xml:"SoftwareComponent"`
}

// decodeXML reads a catalog that may be UTF-16, as Dell's consumer catalogs
// are, into UTF-8 that encoding/xml accepts: the byte-order mark is used to
// decode it, and the declaration's encoding is dropped since the bytes no
// longer are what it says.
func decodeXML(b []byte) []byte {
	if len(b) >= 2 && (b[0] == 0xFF && b[1] == 0xFE || b[0] == 0xFE && b[1] == 0xFF) {
		big := b[0] == 0xFE
		b = b[2:]
		u := make([]uint16, len(b)/2)
		for i := range u {
			if big {
				u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
			} else {
				u[i] = uint16(b[2*i+1])<<8 | uint16(b[2*i])
			}
		}
		b = []byte(string(utf16.Decode(u)))
	}
	s := string(b)
	if strings.HasPrefix(s, "\ufeff") {
		s = s[len("\ufeff"):]
	}
	if i := strings.Index(s, "?>"); strings.HasPrefix(s, "<?xml") && i > 0 {
		s = `<?xml version="1.0"?>` + s[i+2:]
	}
	return []byte(s)
}

// parseAlienwareIndex lists every Alienware system in the index.
func parseAlienwareIndex(b []byte) ([]alienwareSystem, error) {
	var idx dellIndex
	if err := xml.Unmarshal(decodeXML(b), &idx); err != nil {
		return nil, fmt.Errorf("parsing Dell's consumer catalog index: %w", err)
	}
	var out []alienwareSystem
	for _, g := range idx.Groups {
		sum := ""
		for _, h := range g.Manifest.Hashes {
			if strings.EqualFold(h.Algorithm, "SHA256") {
				sum = strings.ToLower(strings.TrimSpace(h.Value))
			}
		}
		for _, br := range g.Brands {
			if !alienwareBrands[strings.ToUpper(br.Prefix)] {
				continue
			}
			for _, m := range br.Models {
				name := strings.Join(strings.Fields(m.Display), " ")
				if name == "" || g.Manifest.Path == "" || sum == "" {
					continue
				}
				out = append(out, alienwareSystem{Model: name, SystemID: m.SystemID, Path: g.Manifest.Path, SHA256: sum})
			}
		}
	}
	return out, nil
}

// dellOSCodes are the catalog's codes for an OS. Windows 11 appears as both
// W21H4 and W21P4; Windows 10 under its own codes and the older Windows10.0.
func dellOSCodes(osName string) map[string]bool {
	if osName == "win10" {
		return map[string]bool{"W10H4": true, "W10P4": true, "WINDOWS10.0": true}
	}
	return map[string]bool{"W21H4": true, "W21P4": true}
}

// alienwareComponents picks a model's driver packages out of its catalogs:
// drivers for the OS and architecture, and of each driver only its newest
// release.
//
// A catalog keeps every release a driver has had -- the m16 R2's lists ten
// Intel graphics drivers, a gigabyte each -- and nothing marks which is
// current. Names change between releases ("Killer Wireless 1750" becomes
// "Intel Killer BE1775/BE1750"), and so do the device lists, a subsystem ID
// added here and there, so neither the name nor the exact device list says
// that two packages are the same driver. What does is sharing a device: two
// packages in the same category that install for the same PCI device are two
// releases of one driver, and only the newer is kept.
func alienwareComponents(model string, catalogs [][]byte, osName, arch string) ([]Pack, error) {
	codes := dellOSCodes(osName)
	type candidate struct {
		pack     Pack
		when     string
		category string
		devices  []string
	}
	var cands []candidate
	for _, b := range catalogs {
		var c dellGroupCatalog
		if err := xml.Unmarshal(decodeXML(b), &c); err != nil {
			return nil, fmt.Errorf("parsing an Alienware catalog: %w", err)
		}
		for _, sc := range c.Components {
			if !strings.EqualFold(sc.Type.Value, "DRVR") || sc.Path == "" {
				continue
			}
			osOK := false
			for _, o := range sc.OS {
				if codes[strings.ToUpper(o.Code)] && (o.Arch == "" || strings.EqualFold(o.Arch, arch)) {
					osOK = true
				}
			}
			if !osOK {
				continue
			}
			var pack Pack
			for _, h := range sc.Hashes {
				switch strings.ToUpper(h.Algorithm) {
				case "SHA256":
					pack.SHA256 = strings.ToLower(strings.TrimSpace(h.Value))
				case "SHA1":
					pack.SHA1 = strings.ToLower(strings.TrimSpace(h.Value))
				case "MD5":
					pack.MD5 = strings.ToLower(strings.TrimSpace(h.Value))
				}
			}
			if pack.SHA256 == "" {
				// Every package is pinned by the hash Dell publishes for it;
				// one without cannot be staged the way the others are.
				continue
			}
			name := strings.Join(strings.Fields(sc.Name), " ")
			version := strings.TrimSpace(sc.Vendor)
			if sc.DellVersion != "" {
				version = strings.TrimSpace(version + " " + sc.DellVersion)
			}
			pack.Vendor = Alienware
			pack.Model = model
			pack.Component = name
			pack.OS = osName
			pack.Version = version
			pack.Released = sc.DateTime
			pack.URL = "https://downloads.dell.com/" + strings.TrimPrefix(sc.Path, "/")
			pack.Size = sc.Size
			pack.Format = "exe"
			pack.Extract = DellExtract
			devices := pciDevices(sc.Devices, sc.DCHDevices)
			if len(devices) == 0 {
				devices = []string{"name:" + strings.ToLower(name)}
			}
			cands = append(cands, candidate{pack, sc.DateTime, strings.ToUpper(sc.Category.Value), devices})
		}
	}

	// Group releases of one driver: same category, any device in common.
	parent := make([]int, len(cands))
	for i := range parent {
		parent[i] = i
	}
	find := func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	owner := map[string]int{}
	for i, c := range cands {
		for _, d := range c.devices {
			key := c.category + "|" + d
			if j, ok := owner[key]; ok {
				parent[find(i)] = find(j)
			} else {
				owner[key] = i
			}
		}
	}
	newest := map[int]int{}
	for i, c := range cands {
		root := find(i)
		if j, ok := newest[root]; !ok || c.when > cands[j].when {
			newest[root] = i
		}
	}
	out := make([]Pack, 0, len(newest))
	for _, i := range newest {
		out = append(out, cands[i].pack)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Component != out[j].Component {
			return out[i].Component < out[j].Component
		}
		return out[i].URL < out[j].URL
	})
	return out, nil
}

// pciDevices lists what a package installs for: its PCI devices as
// vendor:device, and Dell's component IDs. Subsystem IDs are left out -- they
// are what changes from one release of a driver to the next, while the chip it
// drives stays the same. Component IDs cover what PCI IDs cannot: a Bluetooth
// radio is a USB device, and its releases share no PCI ID at all.
func pciDevices(lists ...[]struct {
	ComponentID string `xml:"componentID,attr"`
	PCI         []struct {
		Vendor    string `xml:"vendorID,attr"`
		Device    string `xml:"deviceID,attr"`
		SubVendor string `xml:"subVendorID,attr"`
		SubDevice string `xml:"subDeviceID,attr"`
	} `xml:"PCIInfo"`
}) []string {
	seen := map[string]bool{}
	var ids []string
	for _, l := range lists {
		for _, d := range l {
			for _, p := range d.PCI {
				id := strings.ToUpper(p.Vendor + ":" + p.Device)
				if p.Device != "" && !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
			if id := "component:" + strings.TrimSpace(d.ComponentID); d.ComponentID != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	sort.Strings(ids)
	return ids
}

// systems reads the index.
func (f *alienwareFeed) systems(ctx context.Context) ([]alienwareSystem, error) {
	cab, err := f.cache.file(ctx, "CatalogIndexPC.cab", dellIndexPCURL)
	if err != nil {
		return nil, err
	}
	b, err := f.cache.expandOne(ctx, cab, "CatalogIndexPC-x")
	if err != nil {
		return nil, err
	}
	return parseAlienwareIndex(b)
}

// catalog fetches one system's catalog, checked against the SHA-256 the index
// publishes for it, and cached under that hash so a new catalog is a new file.
func (f *alienwareFeed) catalog(ctx context.Context, s alienwareSystem) ([]byte, error) {
	name := "alienware-" + s.SystemID + "-" + s.SHA256[:16] + ".cab"
	path := filepath.Join(f.cache.Dir, name)
	if err := os.MkdirAll(f.cache.Dir, 0o755); err != nil {
		return nil, err
	}
	if !fileHasSHA256(path, s.SHA256) {
		sum, err := fetch.Download(ctx, "https://downloads.dell.com/"+strings.TrimPrefix(s.Path, "/"), path, nil)
		if err != nil {
			return nil, fmt.Errorf("fetching the %s catalog: %w", s.Model, err)
		}
		if !strings.EqualFold(sum, s.SHA256) {
			os.Remove(path)
			return nil, fmt.Errorf("the %s catalog does not match the SHA-256 Dell's index publishes for it — refusing", s.Model)
		}
	}
	return f.cache.expandOne(ctx, path, strings.TrimSuffix(name, ".cab")+"-x")
}

func fileHasSHA256(path, want string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(b)
	return strings.EqualFold(hex.EncodeToString(sum[:]), want)
}

// forModel fetches the catalogs of every system ID named model -- one model is
// often several IDs -- a few at a time.
func (f *alienwareFeed) forModel(ctx context.Context, systems []alienwareSystem) ([][]byte, error) {
	out := make([][]byte, len(systems))
	errs := make([]error, len(systems))
	var wg sync.WaitGroup
	limit := make(chan struct{}, 4)
	for i, s := range systems {
		wg.Add(1)
		go func(i int, s alienwareSystem) {
			defer wg.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			out[i], errs[i] = f.catalog(ctx, s)
		}(i, s)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Models lists every Alienware model with drivers for the OS: all of the
// index's Alienware systems, less those whose catalogs hold no driver for it
// -- M17x and its generation are in the index and have nothing for Windows 11.
func (f *alienwareFeed) Models(ctx context.Context, osName, arch string) ([]string, error) {
	q := Query{OS: osName, Arch: arch}
	q.defaults()
	systems, err := f.systems(ctx)
	if err != nil {
		return nil, err
	}
	byModel := map[string][]alienwareSystem{}
	for _, s := range systems {
		byModel[s.Model] = append(byModel[s.Model], s)
	}
	var (
		mu    sync.Mutex
		names []string
		wg    sync.WaitGroup
		errs  []error
	)
	limit := make(chan struct{}, 2)
	for model, ss := range byModel {
		wg.Add(1)
		go func(model string, ss []alienwareSystem) {
			defer wg.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			cats, err := f.forModel(ctx, ss)
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			packs, err := alienwareComponents(model, cats, q.OS, q.Arch)
			if err == nil && len(packs) > 0 {
				mu.Lock()
				names = append(names, model)
				mu.Unlock()
			}
		}(model, ss)
	}
	wg.Wait()
	if len(names) == 0 && len(errs) > 0 {
		return nil, errs[0]
	}
	return sortedUnique(names), nil
}

// Search finds Alienware models by name. Each result stands for the model's
// whole set of drivers: its size is the total, and Components lists them.
func (f *alienwareFeed) Search(ctx context.Context, q Query) ([]Pack, error) {
	q.defaults()
	systems, err := f.systems(ctx)
	if err != nil {
		return nil, err
	}
	byModel := map[string][]alienwareSystem{}
	for _, s := range systems {
		if modelMatches(q.Model, s.Model) || strings.EqualFold(s.SystemID, strings.TrimSpace(q.Model)) {
			byModel[s.Model] = append(byModel[s.Model], s)
		}
	}
	var out []Pack
	for model, ss := range byModel {
		cats, err := f.forModel(ctx, ss)
		if err != nil {
			return nil, err
		}
		comps, err := alienwareComponents(model, cats, q.OS, q.Arch)
		if err != nil {
			return nil, err
		}
		if len(comps) == 0 {
			continue
		}
		p := Pack{Vendor: Alienware, Model: model, OS: q.OS, Format: "exe",
			Version: fmt.Sprintf("%d driver packages", len(comps))}
		for _, s := range ss {
			p.SystemIDs = append(p.SystemIDs, s.SystemID)
		}
		for _, c := range comps {
			p.Size += c.Size
			if c.Released > p.Released {
				p.Released = c.Released
			}
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out, nil
}

// Components is every driver package for exactly this model.
func (f *alienwareFeed) Components(ctx context.Context, q Query) ([]Pack, error) {
	q.defaults()
	systems, err := f.systems(ctx)
	if err != nil {
		return nil, err
	}
	var ss []alienwareSystem
	want := strings.Join(strings.Fields(strings.ToLower(q.Model)), " ")
	for _, s := range systems {
		if strings.ToLower(s.Model) == want {
			ss = append(ss, s)
		}
	}
	if len(ss) == 0 {
		return nil, fmt.Errorf("no Alienware model is named %q in Dell's catalog", q.Model)
	}
	cats, err := f.forModel(ctx, ss)
	if err != nil {
		return nil, err
	}
	return alienwareComponents(ss[0].Model, cats, q.OS, q.Arch)
}

// expandOne expands a cab into subdir (once) and returns its catalog XML,
// which in Dell's consumer catalogs is UTF-16.
func (c *Cache) expandOne(ctx context.Context, cab, subdir string) ([]byte, error) {
	dir := filepath.Join(c.Dir, subdir)
	cabSt, err := os.Stat(cab)
	if err != nil {
		return nil, err
	}
	if found := findXML(dir); found != "" {
		if st, err := os.Stat(found); err == nil && !st.ModTime().Before(cabSt.ModTime()) {
			return os.ReadFile(found)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := helpers.ExpandCab(ctx, c.HelpersDir, cab, dir); err != nil {
		return nil, err
	}
	found := findXML(dir)
	if found == "" {
		return nil, fmt.Errorf("%s did not expand to a catalog", filepath.Base(cab))
	}
	return os.ReadFile(found)
}
