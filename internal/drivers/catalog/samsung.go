package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Samsung's Galaxy Book download center is a JSON API that answers without a
// login: the marketing names, the model codes under each, and for a model code
// its drivers -- each with its OS, exact size, release date and a category.
// Most models since the Galaxy Book3 have a "Windows11 DriverPack", category
// 720: one zip of INFs grouped by device, which Samsung's own readme says to
// install with pnputil. That is the pack. A model without one -- the Galaxy
// Book2, say -- has its drivers as separate zips of INFs, and those are swept
// instead.
//
// Samsung publishes no hash for any of these, so each is pinned by its SHA-256
// the first time it is downloaded.
const (
	samsungAPI     = "https://searchapi.samsung.com/v6/front/gbdc/galaxybook/"
	samsungContent = "https://downloadcenter.samsung.com/content/"

	samsungDriverPack   = "720"
	samsungPEDriverPack = "721" // for WinPE, not for an installed Windows
	samsungBIOS         = "622"
)

type samsungFeed struct{ cache *Cache }

func (f *samsungFeed) Vendor() Vendor { return Samsung }

// samsungModel is one base model: the marketing name and the part of the
// model code before the regional suffix, which is what a person would call
// the machine, with the full codes it is sold under.
type samsungModel struct {
	Name  string
	Codes []string
}

func (f *samsungFeed) getJSON(ctx context.Context, name, u string, v any) error {
	p, err := f.cache.file(ctx, name, u)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		os.Remove(p)
		return fmt.Errorf("Samsung's download center answered %s with something that is not its JSON: %w", name, err)
	}
	return nil
}

// models lists every base model the US download center has: its marketing
// names, then the model codes under each, one request per name.
func (f *samsungFeed) models(ctx context.Context) ([]samsungModel, error) {
	var names struct {
		Response struct {
			ResultData struct {
				List []struct {
					Name string `json:"marketingName"`
				} `json:"marketingNameList"`
			} `json:"resultData"`
		} `json:"response"`
	}
	if err := f.getJSON(ctx, "samsung-names-us.json", samsungAPI+"marketingNameList?stage=admin&siteCode=us", &names); err != nil {
		return nil, err
	}
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		codes = map[string][]string{}
		errs  []error
		limit = make(chan struct{}, 8)
	)
	for _, n := range names.Response.ResultData.List {
		name := strings.TrimSpace(n.Name)
		if name == "" {
			continue
		}
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			var list struct {
				Response struct {
					ResultData struct {
						List []struct {
							Code string `json:"modelCode"`
						} `json:"modelCodeList"`
					} `json:"resultData"`
				} `json:"response"`
			}
			err := f.getJSON(ctx, "samsung-codes-us-"+slug(name)+".json",
				samsungAPI+"modelCodeList?stage=front&siteCode=us&marketingName="+url.QueryEscape(name), &list)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			for _, c := range list.Response.ResultData.List {
				codes[name] = append(codes[name], c.Code)
			}
		}(name)
	}
	wg.Wait()
	return groupSamsungModels(codes), listResult(len(codes), errs, len(names.Response.ResultData.List))
}

// groupSamsungModels turns marketing names and their model codes into base
// models: "Galaxy Book4 Pro" covers the 14-inch NP940XGK and the 16-inch
// NP960XGK, each sold under several regional codes, and a person picks the
// machine, not the region.
func groupSamsungModels(codes map[string][]string) []samsungModel {
	byName := map[string]*samsungModel{}
	for marketing, list := range codes {
		for _, code := range list {
			code = strings.TrimSpace(code)
			if code == "" {
				continue
			}
			base, _, _ := strings.Cut(code, "-")
			name := strings.TrimSpace(marketing) + " (" + base + ")"
			m := byName[name]
			if m == nil {
				m = &samsungModel{Name: name}
				byName[name] = m
			}
			m.Codes = append(m.Codes, code)
		}
	}
	out := make([]samsungModel, 0, len(byName))
	for _, m := range byName {
		sort.Strings(m.Codes)
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// samsungDetails is one model code's downloads.
type samsungDetails struct {
	Response struct {
		ResultData struct {
			Downloads struct {
				Drivers []struct {
					Name        string `json:"name"`
					Version     string `json:"version"`
					FileSize    string `json:"fileSize"`
					ReleaseDate string `json:"releaseDate"`
					FilePath    string `json:"filePath"`
					OS          string `json:"osName"`
					Category    string `json:"category1"`
				} `json:"driverAndFirmwares"`
			} `json:"downloadContents"`
		} `json:"resultData"`
	} `json:"response"`
}

func (f *samsungFeed) details(ctx context.Context, code string) (*samsungDetails, error) {
	var d samsungDetails
	err := f.getJSON(ctx, "samsung-"+code+".json", samsungAPI+"modelDetailConts?siteCode=us&modelCode="+url.QueryEscape(code), &d)
	return &d, err
}

// samsungComponents picks a model's drivers for the OS: its driver pack when
// it has one, and otherwise each of its driver zips. Never the WinPE pack, the
// BIOS or anything that is not a zip.
func samsungComponents(model string, d *samsungDetails, osName string) []Pack {
	wantOS := "Windows " + strings.TrimPrefix(osName, "win")
	var pack []Pack
	var parts []Pack
	for _, it := range d.Response.ResultData.Downloads.Drivers {
		if !strings.EqualFold(strings.TrimSpace(it.OS), wantOS) {
			continue
		}
		u, err := url.Parse(it.FilePath)
		if err != nil {
			continue
		}
		vpath := u.Query().Get("VPath")
		if vpath == "" || !strings.EqualFold(path.Ext(vpath), ".zip") {
			continue
		}
		size, _ := strconv.ParseInt(strings.TrimSpace(it.FileSize), 10, 64)
		released, _, _ := strings.Cut(it.ReleaseDate, "T")
		p := Pack{
			Vendor:    Samsung,
			Model:     model,
			Component: strings.Join(strings.Fields(it.Name), " "),
			OS:        osName,
			Version:   strings.TrimSpace(it.Version),
			Released:  released,
			URL:       samsungContent + strings.TrimPrefix(vpath, "/"),
			Size:      size,
			Format:    "zip",
			Install:   "pnputil-sweep",
		}
		switch it.Category {
		case samsungDriverPack:
			pack = append(pack, p)
		case samsungPEDriverPack, samsungBIOS:
		default:
			parts = append(parts, p)
		}
	}
	if len(pack) > 0 {
		sort.Slice(pack, func(i, j int) bool { return pack[i].Released > pack[j].Released })
		return pack[:1]
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Component < parts[j].Component })
	return parts
}

// forModel finds the drivers of a base model: those of the first of its codes
// that has any for the OS. Regional codes of one model carry the same drivers.
func (f *samsungFeed) forModel(ctx context.Context, m samsungModel, osName string) ([]Pack, error) {
	var firstErr error
	for _, code := range m.Codes {
		d, err := f.details(ctx, code)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if comps := samsungComponents(m.Name, d, osName); len(comps) > 0 {
			return comps, nil
		}
	}
	return nil, firstErr
}

// Models lists Galaxy Book models with drivers for the OS.
func (f *samsungFeed) Models(ctx context.Context, osName, arch string) ([]string, error) {
	q := Query{OS: osName, Arch: arch}
	q.defaults()
	if q.Arch != "x64" {
		return nil, nil
	}
	models, err := f.models(ctx)
	if err != nil && !IsPartial(err) {
		return nil, err
	}
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		names []string
		errs  []error
		limit = make(chan struct{}, 8)
	)
	for _, m := range models {
		wg.Add(1)
		go func(m samsungModel) {
			defer wg.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			comps, err := f.forModel(ctx, m, q.OS)
			mu.Lock()
			defer mu.Unlock()
			if len(comps) > 0 {
				names = append(names, m.Name)
			} else if err != nil {
				errs = append(errs, err)
			}
		}(m)
	}
	wg.Wait()
	if len(errs) > 0 {
		return sortedUnique(names), listResult(len(names), errs, len(models))
	}
	return sortedUnique(names), err
}

// Search finds Galaxy Book models by name or model code.
func (f *samsungFeed) Search(ctx context.Context, q Query) ([]Pack, error) {
	q.defaults()
	models, err := f.models(ctx)
	if err != nil && !IsPartial(err) {
		return nil, err
	}
	var out []Pack
	for _, m := range models {
		if !modelMatches(q.Model, m.Name) && !strings.EqualFold(strings.TrimSpace(q.Model), m.Name) {
			continue
		}
		comps, err := f.forModel(ctx, m, q.OS)
		if err != nil || len(comps) == 0 {
			continue
		}
		p := Pack{Vendor: Samsung, Model: m.Name, OS: q.OS, SystemIDs: m.Codes, Format: "zip",
			Version: fmt.Sprintf("%d driver packages", len(comps))}
		for _, c := range comps {
			p.Size += c.Size
			if c.Released > p.Released {
				p.Released = c.Released
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// Components is the drivers for exactly this model.
func (f *samsungFeed) Components(ctx context.Context, q Query) ([]Pack, error) {
	q.defaults()
	models, err := f.models(ctx)
	if err != nil && !IsPartial(err) {
		return nil, err
	}
	want := strings.Join(strings.Fields(strings.ToLower(q.Model)), " ")
	for _, m := range models {
		if strings.ToLower(m.Name) == want {
			return f.forModel(ctx, m, q.OS)
		}
	}
	return nil, fmt.Errorf("Samsung lists no Galaxy Book named %q", q.Model)
}
