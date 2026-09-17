package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ASUS publishes no driver-pack catalog, but its support site is built on a
// JSON API that answers without a login: product lines, their series, the
// products in each, and for a product its drivers -- each with a SHA-256, the
// hardware IDs it covers, and the command ASUS's own updater runs it with.
//
// Intel handed the NUC line to ASUS in 2023, and every NUC driver now lives on
// the same API. NUCs are listed apart, because what they offer differs: an
// Intel "INF Driver Pack", a zip of INFs for pnputil, the same kind of pack as
// Dell's. ASUS laptops and desktops have no pack; their drivers are separate
// Inno Setup installers, run with the switches ASUS publishes for each, and
// only on the model they are for.
const (
	asusAPI      = "https://www.asus.com/support/api/product.asmx/GetPDLevel?website=us"
	asusDrivers  = "https://www.asus.com/support/webapi/ProductV2/GetPDDrivers?website=us&model=&pdhashedid=&cpu=&pdid="
	asusDownload = "https://dlcdnets.asus.com"
	asusWin11ID  = "52" // Windows 11 64-bit in ASUS's OS numbering
	asusNUCLine  = "12031"
)

// asusLines are the product lines that hold computers. ROG also holds mice
// and headsets, so its series are filtered by name.
var asusLines = []string{
	"155",   // Laptops
	"1541",  // Mini PCs
	"1464",  // Tower PCs
	"1618",  // All-in-One PCs
	"2696",  // ROG
	"10505", // TUF Gaming
	"11933", // Handhelds
	"4313",  // Business laptops
	"4390",  // Business desktops
	"9433",  // Business mini PCs
	"4005",  // Business all-in-ones
}

var asusROGComputers = regexp.MustCompile(`(?i)laptop|desktop|handheld|notebook`)

type asusFeed struct {
	cache *Cache
	nuc   bool
}

func (f *asusFeed) Vendor() Vendor {
	if f.nuc {
		return NUC
	}
	return ASUS
}

// asusProduct is one product the API lists.
type asusProduct struct {
	ID   string `json:"PDId"`
	Name string `json:"PDName"`
}

func (f *asusFeed) getJSON(ctx context.Context, name, url string, v any) error {
	p, err := f.cache.file(ctx, name, url)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		os.Remove(p) // a half-written or HTML error page; fetch again next time
		return fmt.Errorf("ASUS's support API answered %s with something that is not its JSON: %w", name, err)
	}
	return nil
}

// products walks the lines, their series and the series' products.
func (f *asusFeed) products(ctx context.Context) ([]asusProduct, error) {
	lines := asusLines
	if f.nuc {
		lines = []string{asusNUCLine}
	}
	type level struct {
		Result struct {
			ProductLevel map[string]struct {
				Items []struct {
					ID   string `json:"Id"`
					Name string `json:"Name"`
				} `json:"Items"`
			} `json:"ProductLevel"`
			Product []asusProduct `json:"Product"`
		} `json:"Result"`
	}
	var series []string
	for _, line := range lines {
		var l level
		if err := f.getJSON(ctx, "asus-line-"+line+".json", asusAPI+"&type=1&productflag=0&typeid="+line, &l); err != nil {
			return nil, err
		}
		for _, group := range l.Result.ProductLevel {
			for _, s := range group.Items {
				if line == "2696" && !asusROGComputers.MatchString(s.Name) {
					continue
				}
				series = append(series, s.ID)
			}
		}
	}
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		out   []asusProduct
		errs  []error
		limit = make(chan struct{}, 8)
		seen  = map[string]bool{}
	)
	for _, s := range series {
		wg.Add(1)
		go func(s string) {
			defer wg.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			var l level
			if err := f.getJSON(ctx, "asus-series-"+s+".json", asusAPI+"&type=2&productflag=1&typeid="+s, &l); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			mu.Lock()
			for _, p := range l.Result.Product {
				p.Name = strings.Join(strings.Fields(p.Name), " ")
				if p.ID != "" && p.Name != "" && !seen[p.ID] {
					seen[p.ID] = true
					out = append(out, p)
				}
			}
			mu.Unlock()
		}(s)
	}
	wg.Wait()
	return out, listResult(len(out), errs, len(series))
}

// asusDriverList is one product's drivers for one OS.
type asusDriverList struct {
	Status string `json:"Status"`
	Result *struct {
		Model string `json:"Model"`
		Obj   []struct {
			Name  string `json:"Name"`
			Files []struct {
				Title       string `json:"Title"`
				Version     string `json:"Version"`
				FileSize    string `json:"FileSize"`
				ReleaseDate string `json:"ReleaseDate"`
				DownloadURL struct {
					Global string `json:"Global"`
				} `json:"DownloadUrl"`
				Hardware []struct {
					ID string `json:"hardwareid"`
				} `json:"HardwareInfoList"`
				ExeModule string `json:"ExeModule"`
				SHA256    string `json:"sha256"`
			} `json:"Files"`
		} `json:"Obj"`
	} `json:"Result"`
}

func (f *asusFeed) driverList(ctx context.Context, pdid string) (*asusDriverList, error) {
	var d asusDriverList
	err := f.getJSON(ctx, "asus-drivers-"+pdid+"-"+asusWin11ID+".json", asusDrivers+pdid+"&osid="+asusWin11ID, &d)
	return &d, err
}

var (
	asusHex64     = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	asusSize      = regexp.MustCompile(`(?i)^\s*([0-9.]+)\s*(KB|MB|GB)`)
	asusVersionly = regexp.MustCompile(`\S*\d\S*`)
	asusDevice    = regexp.MustCompile(`(?i)^([A-Z]+\\(?:VEN_[0-9A-F]+&DEV_[0-9A-F]+|[^&\\]+))`)
)

func asusBytes(s string) int64 {
	m := asusSize.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseFloat(m[1], 64)
	switch strings.ToUpper(m[2]) {
	case "GB":
		n *= 1 << 30
	case "MB":
		n *= 1 << 20
	default:
		n *= 1 << 10
	}
	return int64(n)
}

// asusComponents picks the drivers to stage out of a product's list.
//
// For a NUC that is its INF driver packs, and only if it has none, its other
// zips of drivers. For anything else it is its driver installers: entries that
// name the hardware they are for (utilities, the Microsoft Store links and
// ASUS's recovery tools name none), that ASUS publishes a SHA-256 and a silent
// command for, as installers run with that command -- or, for the few that are
// zips, swept. Of each driver only the newest release is kept: ASUS lists every
// release, and two releases are recognised as one driver by sharing a device.
func asusComponents(model string, list *asusDriverList, osName string, nuc bool) []Pack {
	if list == nil || list.Result == nil || osName != "win11" {
		return nil
	}
	gate := strings.TrimSpace(list.Result.Model)
	type candidate struct {
		pack     Pack
		when     string
		category string
		keys     []string
	}
	var cands, fallback []candidate
	for _, cat := range list.Result.Obj {
		for _, fl := range cat.Files {
			url := fl.DownloadURL.Global
			if !strings.HasPrefix(url, "/pub/") {
				continue // a Microsoft Store link, or nothing downloadable
			}
			ext := strings.ToLower(path.Ext(url))
			sum := strings.ToLower(strings.TrimSpace(fl.SHA256))
			if !asusHex64.MatchString(sum) {
				sum = ""
			}
			title := strings.Join(strings.Fields(fl.Title), " ")
			p := Pack{
				Vendor:    ASUS,
				Model:     model,
				Component: title,
				OS:        osName,
				Version:   strings.TrimSpace(fl.Version),
				Released:  strings.ReplaceAll(fl.ReleaseDate, "/", "-"),
				URL:       asusDownload + url,
				Size:      asusBytes(fl.FileSize),
				SHA256:    sum,
			}
			if nuc {
				p.Vendor = NUC
			}
			var keys []string
			for _, h := range fl.Hardware {
				if m := asusDevice.FindStringSubmatch(strings.TrimSpace(h.ID)); m != nil {
					keys = append(keys, strings.ToUpper(m[1]))
				}
			}
			if len(keys) == 0 {
				keys = []string{"title:" + strings.ToLower(asusVersionly.ReplaceAllString(title, ""))}
			}
			c := candidate{p, fl.ReleaseDate, strings.ToLower(cat.Name), keys}

			switch {
			case nuc:
				if ext != ".zip" {
					continue
				}
				c.pack.Format, c.pack.Install = "zip", "pnputil-sweep"
				low := strings.ToLower(cat.Name + " " + title)
				if strings.Contains(low, "inf driver pack") {
					cands = append(cands, c)
				} else if !strings.Contains(low, "bios") && !strings.Contains(low, "firmware") && !strings.Contains(low, "utilit") {
					fallback = append(fallback, c)
				}
			default:
				if len(fl.Hardware) == 0 || sum == "" {
					continue
				}
				switch ext {
				case ".exe":
					_, switches, ok := strings.Cut(fl.ExeModule, "%%")
					args := strings.Fields(switches)
					if !ok || len(args) == 0 {
						continue // no published way to run it unattended
					}
					c.pack.Format, c.pack.Install, c.pack.Args, c.pack.Gate = "exe", "exe", args, gate
				case ".zip":
					c.pack.Format, c.pack.Install = "zip", "pnputil-sweep"
				default:
					continue
				}
				cands = append(cands, c)
			}
		}
	}
	if nuc && len(cands) == 0 {
		cands = fallback
	}

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
		for _, k := range c.keys {
			key := c.category + "|" + k
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
	return out
}

// Models lists ASUS computers, or NUCs. NUCs are few enough to keep only those
// with drivers for the OS; ASUS lists nearly four thousand products, and
// checking each would be four thousand requests, so an ASUS model without
// drivers for the OS says so when it is chosen instead.
func (f *asusFeed) Models(ctx context.Context, osName, arch string) ([]string, error) {
	q := Query{OS: osName, Arch: arch}
	q.defaults()
	if q.Arch != "x64" || q.OS != "win11" {
		return nil, nil
	}
	products, err := f.products(ctx)
	if err != nil && !IsPartial(err) {
		return nil, err
	}
	if !f.nuc {
		names := make([]string, 0, len(products))
		for _, p := range products {
			names = append(names, p.Name)
		}
		return sortedUnique(names), err
	}
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		names []string
		errs  []error
		limit = make(chan struct{}, 8)
	)
	for _, p := range products {
		wg.Add(1)
		go func(p asusProduct) {
			defer wg.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			list, err := f.driverList(ctx, p.ID)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			} else if len(asusComponents(p.Name, list, q.OS, true)) > 0 {
				names = append(names, p.Name)
			}
		}(p)
	}
	wg.Wait()
	if len(errs) > 0 {
		return sortedUnique(names), listResult(len(names), errs, len(products))
	}
	return sortedUnique(names), err
}

// matching finds the products named model, exactly, then loosely.
func (f *asusFeed) matching(ctx context.Context, model string, exact bool) ([]asusProduct, error) {
	products, err := f.products(ctx)
	if err != nil && !IsPartial(err) {
		return nil, err
	}
	want := strings.Join(strings.Fields(strings.ToLower(model)), " ")
	var out []asusProduct
	for _, p := range products {
		if strings.ToLower(p.Name) == want || !exact && modelMatches(model, p.Name) {
			out = append(out, p)
		}
	}
	return out, nil
}

// Search finds products by name; each result stands for the product's drivers.
func (f *asusFeed) Search(ctx context.Context, q Query) ([]Pack, error) {
	q.defaults()
	products, err := f.matching(ctx, q.Model, false)
	if err != nil {
		return nil, err
	}
	var out []Pack
	for _, p := range products {
		list, err := f.driverList(ctx, p.ID)
		if err != nil {
			continue
		}
		comps := asusComponents(p.Name, list, q.OS, f.nuc)
		if len(comps) == 0 {
			continue
		}
		pack := Pack{Vendor: f.Vendor(), Model: p.Name, OS: q.OS, SystemIDs: []string{p.ID},
			Version: fmt.Sprintf("%d driver packages", len(comps))}
		for _, c := range comps {
			pack.Size += c.Size
			if c.Released > pack.Released {
				pack.Released = c.Released
			}
		}
		out = append(out, pack)
	}
	return out, nil
}

// Components is every driver package for exactly this product. Two products
// can share a name -- ASUS lists some NUCs under both Intel's and its own --
// and they share drivers, so the first with drivers is used.
func (f *asusFeed) Components(ctx context.Context, q Query) ([]Pack, error) {
	q.defaults()
	if q.OS != "win11" {
		return nil, fmt.Errorf("DSKY reads ASUS drivers for Windows 11 only")
	}
	products, err := f.matching(ctx, q.Model, true)
	if err != nil {
		return nil, err
	}
	if len(products) == 0 {
		return nil, fmt.Errorf("ASUS lists no product named %q", q.Model)
	}
	for _, p := range products {
		list, err := f.driverList(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		if comps := asusComponents(p.Name, list, q.OS, f.nuc); len(comps) > 0 {
			return comps, nil
		}
	}
	return nil, nil
}
