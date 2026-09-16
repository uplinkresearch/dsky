package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/uplinkresearch/dsky/internal/driverresolve"
	"github.com/uplinkresearch/dsky/internal/drivers/catalog"
	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/manifest"
	"github.com/uplinkresearch/dsky/internal/recipe"
	"github.com/uplinkresearch/dsky/internal/workspace"
)

// driversSearch queries a vendor catalog (dell/lenovo/hp by model) or the
// Microsoft Update Catalog (by hardware ID) and optionally adds a result
// to the workspace as a pinned, self-describing driver-pack manifest.
func driversSearch(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("drivers search", flag.ContinueOnError)
	osName := fs.String("os", "win11", "target OS: win11 | win10")
	limit := fs.Int("limit", 15, "max results to show")
	add := fs.Bool("add", false, "add a result to the workspace (manifest + library pull)")
	pick := fs.Int("pick", 1, "which result --add takes (1 = first)")
	noPull := fs.Bool("no-pull", false, "with --add: write the manifest but don't download yet")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("drivers search <%s|mscatalog> <model or hardware-id> [--add] [--pick N]", modelFeedNames())
	}
	lib, err := env.library()
	if err != nil {
		return err
	}
	feed, err := catalog.FeedFor(fs.Arg(0), catalog.NewCache(lib.HelpersDir()))
	if err != nil {
		return err
	}
	q := catalog.Query{OS: *osName}
	if feed.Vendor() == catalog.MSCatalog {
		q.HWID = fs.Arg(1)
	} else {
		q.Model = fs.Arg(1)
	}
	fmt.Printf("searching %s for %q (%s)...\n", feed.Vendor(), fs.Arg(1), *osName)
	packs, err := feed.Search(ctx, q)
	if err != nil {
		return err
	}
	if len(packs) == 0 {
		return fmt.Errorf("no %s driver packs match %q for %s", feed.Vendor(), fs.Arg(1), *osName)
	}
	printPacks(packs, *limit)
	if !*add {
		fmt.Printf("\nAdd one:  dsky drivers search %s %q --add --pick N\n", fs.Arg(0), fs.Arg(1))
		return nil
	}
	if *pick < 1 || *pick > len(packs) {
		return fmt.Errorf("--pick %d is out of range (1..%d)", *pick, len(packs))
	}
	ws, err := env.workspace()
	if err != nil {
		return err
	}
	ref := manifest.HardwareRef{OS: *osName}
	if feed.Vendor() == catalog.MSCatalog {
		ref.HWID = fs.Arg(1)
	} else {
		ref.Vendor = string(feed.Vendor())
		ref.Model = packs[*pick-1].Model
	}
	prog := &stageProgress{}
	// A vendor whose drivers are many packages has no one pack to add: the
	// search result stands for the model, and adding it adds every package.
	if _, ok := feed.(catalog.ComponentFeed); ok {
		spec := []recipe.HardwareSpec{{Vendor: ref.Vendor, Model: ref.Model, OS: *osName}}
		var res *driverresolve.Resolved
		if *noPull {
			res, err = driverresolve.Plan(ctx, ws, lib, spec, true, prog.report)
		} else {
			res, err = driverresolve.Resolve(ctx, ws, lib, spec, true, prog.report)
		}
		prog.finish()
		if err != nil {
			return err
		}
		fmt.Printf("\nAdded %d driver packages for %s. A recipe picks them up via:\n", len(res.Packs), ref.Model)
		fmt.Printf("  windows:\n    hardware:\n      - { vendor: %s, model: %q }\n", ref.Vendor, ref.Model)
		return nil
	}
	id, err := driverresolve.AddPack(ctx, ws, lib, feed, packs[*pick-1], ref, !*noPull, prog.report)
	prog.finish()
	if err != nil {
		return err
	}
	fmt.Printf("\nAdded %s. A recipe picks it up via:\n", id)
	if ref.HWID != "" {
		fmt.Printf("  windows:\n    hardware:\n      - { hwids: [%q] }\n", ref.HWID)
	} else {
		fmt.Printf("  windows:\n    hardware:\n      - { vendor: %s, model: %q }\n", ref.Vendor, ref.Model)
	}
	return nil
}

func printPacks(packs []catalog.Pack, limit int) {
	for i, p := range packs {
		if i >= limit {
			fmt.Printf("  … %d more (raise --limit)\n", len(packs)-limit)
			break
		}
		ver := p.OSVersion
		if ver == "" || ver == "*" {
			ver = p.OS
		}
		size := "?"
		if p.Size > 0 {
			size = fmt.Sprintf("%d MiB", p.Size>>20)
			if p.Size < 1<<20 {
				size = fmt.Sprintf("%d KiB", p.Size>>10)
			}
		}
		fmt.Printf("  %2d. %-46s %-6s %-10s %-9s %-4s %s\n", i+1, trunc(p.Model, 46), ver, p.Released, size, p.Format, trunc(p.Version, 14))
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// resolveHardware fetches a driver pack for every windows.hardware entry of a
// recipe. Strict: a hand-authored entry whose hardware no feed covers is an
// authoring mistake, not something to quietly skip.
func resolveHardware(ctx context.Context, ws *workspace.Workspace, lib *library.Library, r *recipe.Recipe) error {
	if r.Windows == nil || len(r.Windows.Hardware) == 0 {
		return nil
	}
	prog := &stageProgress{}
	_, err := driverresolve.Resolve(ctx, ws, lib, r.Windows.Hardware, true, prog.report)
	prog.finish()
	return err
}

func driversResolve(ctx context.Context, env *Env, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("drivers resolve <recipe-id>")
	}
	ws, err := env.workspace()
	if err != nil {
		return err
	}
	lib, err := env.library()
	if err != nil {
		return err
	}
	r, err := ws.Recipe(args[0])
	if err != nil {
		return err
	}
	if r.Windows == nil || len(r.Windows.Hardware) == 0 {
		fmt.Println("recipe has no windows.hardware entries — nothing to resolve")
		return nil
	}
	if err := resolveHardware(ctx, ws, lib, r); err != nil {
		return err
	}
	fmt.Println("all hardware entries have driver packs in the library — build with: dsky", r.ID)
	return nil
}

// driversModels lists every model a vendor's catalog has a driver pack for —
// the same list the Install dialog's model picker offers.
func driversModels(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("drivers models", flag.ContinueOnError)
	osName := fs.String("os", "win11", "win11 or win10")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("drivers models <%s> [--os win11|win10]", modelFeedNames())
	}
	lib, err := env.library()
	if err != nil {
		return err
	}
	feed, err := catalog.FeedFor(fs.Arg(0), catalog.NewCache(lib.HelpersDir()))
	if err != nil {
		return err
	}
	lister, ok := feed.(catalog.Lister)
	if !ok {
		return fmt.Errorf("%s is not organised by computer model — search it by hardware ID instead", fs.Arg(0))
	}
	models, err := lister.Models(ctx, *osName, "x64")
	if err != nil {
		return err
	}
	for _, m := range models {
		fmt.Println(m)
	}
	fmt.Fprintf(os.Stderr, "%d %s models listed for %s\n", len(models), fs.Arg(0), *osName)
	return nil
}

// modelFeedNames is the vendors organised by computer model, for usage lines.
func modelFeedNames() string {
	names := make([]string, len(catalog.ModelFeeds))
	for i, v := range catalog.ModelFeeds {
		names[i] = string(v)
	}
	return strings.Join(names, "|")
}
