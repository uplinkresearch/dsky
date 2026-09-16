package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Microsoft publishes Surface drivers as one MSI per model, on the Download
// Center. The list of models is a table on learn.microsoft.com linking each to
// its download page, and each download page carries its files as JSON in a
// script block. That is the catalog.
//
// The MSI is not installed. An administrative install -- msiexec /a -- unpacks
// it to a folder without running anything, which leaves the model's drivers as
// INFs for pnputil to sweep like any other pack, and leaves out the updater
// tooling a normal install would add. Microsoft publishes no hash for these
// files, so the first download is pinned by its SHA-256.
const surfaceLearnURL = "https://learn.microsoft.com/en-us/surface/manage-surface-driver-and-firmware-updates"

// SurfaceExtract unpacks a Surface MSI with an administrative install. The
// agent runs msiexec for a .msi: /a and the file come first.
var SurfaceExtract = []string{"/qn", "TARGETDIR={dir}"}

type surfaceFeed struct{ cache *Cache }

func (f *surfaceFeed) Vendor() Vendor { return Surface }

var (
	surfaceLink    = regexp.MustCompile(`details\.aspx\?id=(\d+)"[^>]*>([^<]+)</a>`)
	surfaceDetails = regexp.MustCompile(`(?s)window\.__DLCDetails__\s*=\s*(\{.*?\})\s*</script>`)
	surfaceFile    = regexp.MustCompile(`(?i)_Win(10|11)_(\d{5})_([0-9.]+)\.msi$`)
	// Arm models, by name. Most say so -- Snapdragon, SQ3, Pro X -- but the
	// 13-inch Laptop and 12-inch Pro do not.
	surfaceArm = regexp.MustCompile(`(?i)snapdragon|\bsq[123]\b|pro x|12-inch 1st edition|13-inch 1st edition`)
)

// surfaceModel is one row of the Learn page's table.
type surfaceModel struct {
	ID   string
	Name string
}

// parseSurfaceModels reads the model table: every Surface computer linked to a
// Download Center page. Docks and the Surface Hub are in the same table and
// are not computers DSKY installs Windows on.
func parseSurfaceModels(page []byte) []surfaceModel {
	seen := map[string]bool{}
	var out []surfaceModel
	for _, m := range surfaceLink.FindAllStringSubmatch(string(page), -1) {
		name := strings.Join(strings.Fields(html.UnescapeString(m[2])), " ")
		low := strings.ToLower(name)
		if !strings.HasPrefix(low, "surface") || strings.Contains(low, "dock") || strings.Contains(low, "hub") || seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		out = append(out, surfaceModel{ID: m[1], Name: name})
	}
	return out
}

// surfaceArch is the architecture a model's drivers are for.
func surfaceArch(name, file string) string {
	if surfaceArm.MatchString(name) || strings.Contains(strings.ToUpper(file), "_ARM_") {
		return "arm64"
	}
	return "x64"
}

// parseSurfaceDetails picks the MSI for the OS from a download page: the one
// for the newest Windows build, since a page can carry one per build.
func parseSurfaceDetails(page []byte, model, osName, arch string) (Pack, bool) {
	m := surfaceDetails.FindSubmatch(page)
	if m == nil {
		return Pack{}, false
	}
	var d struct {
		View struct {
			Files []struct {
				Name string `json:"name"`
				URL  string `json:"url"`
				Size string `json:"size"`
				Date string `json:"datePublished"`
			} `json:"downloadFile"`
		} `json:"dlcDetailsView"`
	}
	if err := json.Unmarshal(m[1], &d); err != nil {
		return Pack{}, false
	}
	want := strings.TrimPrefix(osName, "win")
	var best Pack
	bestBuild := -1
	for _, f := range d.View.Files {
		fm := surfaceFile.FindStringSubmatch(f.Name)
		if fm == nil || fm[1] != want || !strings.HasPrefix(f.URL, "https://download.microsoft.com/") {
			continue
		}
		if surfaceArch(model, f.Name) != arch {
			continue
		}
		build, _ := strconv.Atoi(fm[2])
		if build <= bestBuild {
			continue
		}
		size, _ := strconv.ParseInt(f.Size, 10, 64)
		bestBuild = build
		best = Pack{
			Vendor:    Surface,
			Model:     model,
			OS:        osName,
			OSVersion: fm[2],
			Version:   fm[3],
			Released:  f.Date,
			URL:       f.URL,
			Size:      size,
			Format:    "msi",
			Install:   "extract-then-sweep",
			Extract:   SurfaceExtract,
		}
	}
	return best, bestBuild >= 0
}

// packs reads every model's download page and returns its MSI for the OS.
func (f *surfaceFeed) packs(ctx context.Context, osName, arch string) ([]Pack, error) {
	learn, err := f.cache.file(ctx, "surface-learn.html", surfaceLearnURL)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(learn)
	if err != nil {
		return nil, err
	}
	models := parseSurfaceModels(b)
	if len(models) == 0 {
		return nil, fmt.Errorf("the Surface driver page lists no models — Microsoft may have changed it")
	}
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		out   []Pack
		errs  []error
		limit = make(chan struct{}, 4)
	)
	for _, m := range models {
		wg.Add(1)
		go func(m surfaceModel) {
			defer wg.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			path, err := f.cache.file(ctx, "surface-"+m.ID+".html", "https://www.microsoft.com/en-us/download/details.aspx?id="+m.ID)
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			page, err := os.ReadFile(path)
			if err != nil {
				return
			}
			if p, ok := parseSurfaceDetails(page, m.Name, osName, arch); ok {
				mu.Lock()
				out = append(out, p)
				mu.Unlock()
			}
		}(m)
	}
	wg.Wait()
	if len(out) == 0 && len(errs) > 0 {
		return nil, errs[0]
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out, nil
}

// Models lists every Surface model with a driver MSI for the OS.
func (f *surfaceFeed) Models(ctx context.Context, osName, arch string) ([]string, error) {
	q := Query{OS: osName, Arch: arch}
	q.defaults()
	packs, err := f.packs(ctx, q.OS, q.Arch)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(packs))
	for _, p := range packs {
		names = append(names, p.Model)
	}
	return sortedUnique(names), nil
}

// Search finds Surface models by name.
func (f *surfaceFeed) Search(ctx context.Context, q Query) ([]Pack, error) {
	q.defaults()
	packs, err := f.packs(ctx, q.OS, q.Arch)
	if err != nil {
		return nil, err
	}
	var out []Pack
	for _, p := range packs {
		if modelMatches(q.Model, p.Model) || strings.EqualFold(strings.TrimSpace(q.Model), p.Model) {
			out = append(out, p)
		}
	}
	return out, nil
}
