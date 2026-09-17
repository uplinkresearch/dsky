package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
)

const appsUsage = `Manage the programs Quick Install can add.

  apps                            list every program
  apps --os ubuntu                list what Ubuntu gets, and where each comes from
  apps --os windows               list what Windows gets
  apps winget <Publisher.Package>  check winget has a package, for --apps winget:<id>
  apps add <installer> [flags]    add your own .msi or .exe
  apps set <id> [flags]           change a name, category or switches
  apps remove <id>                forget one (the file stays in the library)
  apps show <id>                  everything recorded about one

add flags:
  --id <name>        what --apps takes (default: derived from the filename)
  --name "<text>"    what the picker shows (default: the filename)
  --args "<switches>"  silent-install switches (default: /qn for .msi)
  --category "<text>"  picker grouping (default: %q)
  --replace          update an existing id instead of refusing

Example:
  dsky apps add "C:\\installers\\MaculaAgent.msi" --id macula --name "Macula Agent"
  dsky install windows-11 --apps macula,chrome
`

func cmdApps(ctx context.Context, env *Env, args []string) error {
	if len(args) == 0 {
		return listApps(appcatalog.TargetWindows, false)
	}
	switch args[0] {
	case "add":
		return appsAdd(env, args[1:])
	case "set":
		return appsSet(env, args[1:])
	case "remove", "rm", "delete":
		return appsRemove(env, args[1:])
	case "show":
		return appsShow(args[1:])
	case "winget":
		return appsWinget(ctx, args[1:])
	case "help", "--help", "-h":
		fmt.Printf(appsUsage, appcatalog.CustomCategory)
		return nil
	}
	target, one, err := appsOS(args)
	if err != nil {
		return err
	}
	return listApps(target, one)
}

// listApps prints the program list for one operating system, or for both. The
// distinction matters: the list is not the same on each, and a program picked
// for a machine that cannot install it is a machine that comes up without it.
func listApps(target appcatalog.Target, one bool) error {
	switch {
	case !one:
		fmt.Println("Programs `dsky install --apps` can add (comma-separated ids).")
		fmt.Println("A program that installs on only one operating system says so;")
		fmt.Println("`dsky apps --os ubuntu` lists what Ubuntu gets, and where from.")
	case target == appcatalog.TargetUbuntu:
		fmt.Println("Programs `dsky install --apps` can add to Ubuntu, and where each")
		fmt.Println("comes from (comma-separated ids):")
	default:
		fmt.Println("Programs `dsky install --apps` can add to Windows (comma-separated ids):")
	}
	for _, cat := range appcatalog.Categories() {
		var lines []string
		for _, a := range appcatalog.Catalog() {
			if a.Category != cat || (one && !a.InstallsOn(target)) {
				continue
			}
			line := fmt.Sprintf("    %-18s %s", a.ID, a.Name)
			var notes []string
			switch {
			case one && target == appcatalog.TargetUbuntu:
				notes = append(notes, a.Ubuntu.Where())
				if a.Ubuntu.Note != "" {
					notes = append(notes, a.Ubuntu.Note)
				}
			case a.Custom != nil:
				notes = append(notes, fmt.Sprintf("%s · %s%s", a.Custom.Filename,
					strings.ToUpper(a.Custom.Format), argsNote(a.Custom)))
			case !one && !a.InstallsOnWindows():
				notes = append(notes, "Ubuntu only")
			case !one && a.Ubuntu == nil:
				notes = append(notes, "Windows only")
			}
			if target != appcatalog.TargetUbuntu {
				notes = append(notes, a.Labels()...)
			}
			if len(notes) > 0 {
				line += "  (" + strings.Join(notes, "; ") + ")"
			}
			lines = append(lines, line)
		}
		if len(lines) == 0 {
			continue
		}
		fmt.Printf("\n  %s\n", cat)
		for _, l := range lines {
			fmt.Println(l)
		}
	}

	fmt.Println("\nStarter sets (use as set:<id>):")
	for _, s := range appcatalog.Sets {
		// Listed as what this operating system would actually get. A set
		// printed in full for Ubuntu promises Sysinternals and Notepad++,
		// and the machine then arrives without them.
		members := s.Apps
		if one {
			members = nil
			for _, id := range s.Apps {
				if a, ok := appcatalog.Get(id); ok && a.InstallsOn(target) {
					members = append(members, id)
				}
			}
		}
		if len(members) == 0 {
			fmt.Printf("    set:%-14s %s: nothing it lists installs here\n", s.ID, s.Name)
			continue
		}
		fmt.Printf("    set:%-14s %s: %s\n", s.ID, s.Name, strings.Join(members, ", "))
	}
	if one && target == appcatalog.TargetUbuntu {
		fmt.Println("\nOn Ubuntu a starter set brings the programs Ubuntu has; the rest of")
		fmt.Println("the set is Windows software and is left out.")
		fmt.Println("\nExample:")
		fmt.Println("  dsky install ubuntu-26.04-server --apps set:it,vlc,chrome")
		fmt.Println("\napt packages and snaps are installed by Ubuntu's own installer.")
		fmt.Println("Flathub apps and a vendor's repository are added at first boot, which")
		fmt.Println("needs the machine to be online then; everything is logged to")
		fmt.Println("/var/log/dsky-apps.log, and anything that failed is tried again at the")
		fmt.Println("next boot. Programs are offered for Ubuntu Server 24.04 and 26.04 and")
		fmt.Println("Ubuntu Desktop 26.04, whose installer takes DSKY's answers.")
		return nil
	}
	fmt.Println("\nAny other winget package works by its id, as winget:Publisher.Package.")
	fmt.Println("Not in winget, so add their installers yourself (dsky apps add):")
	for _, e := range appcatalog.NotInWinget {
		if !strings.Contains(e.Note, "comes with Windows") {
			fmt.Printf("    %s\n", e.Name)
		}
	}
	fmt.Println("\nExample:")
	fmt.Println("  dsky install windows-11 --drivers --apps set:business,brave,winget:Mozilla.Firefox.ESR")
	fmt.Println("\nBuilt-in programs install at first boot with winget, so the machine")
	fmt.Println("needs to be online then — staging its network driver (--drivers) helps.")
	fmt.Println("Your own installers ride on the stick and need no network.")
	fmt.Println("\nAdd one:  dsky apps add <installer.msi> --id <name>")
	return nil
}

// appsOS reads an --os flag off the front of `dsky apps`.
func appsOS(args []string) (appcatalog.Target, bool, error) {
	if len(args) == 0 {
		return appcatalog.TargetWindows, false, nil
	}
	val := ""
	switch {
	case args[0] == "--os" && len(args) > 1:
		val = args[1]
	case strings.HasPrefix(args[0], "--os="):
		val = strings.TrimPrefix(args[0], "--os=")
	default:
		return appcatalog.TargetWindows, false, fmt.Errorf("unknown apps command %q\n\n"+appsUsage, args[0], appcatalog.CustomCategory)
	}
	switch strings.ToLower(val) {
	case "ubuntu", "linux":
		return appcatalog.TargetUbuntu, true, nil
	case "windows", "win":
		return appcatalog.TargetWindows, true, nil
	}
	return appcatalog.TargetWindows, false, fmt.Errorf("apps --os takes windows or ubuntu, not %q", val)
}

// appsWinget looks a package up in winget's repository.
func appsWinget(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: dsky apps winget <Publisher.Package>")
	}
	id, err := appcatalog.LookupWinget(ctx, args[0])
	if err != nil {
		return fmt.Errorf("%s: %w", args[0], err)
	}
	fmt.Printf("winget has %s. Use it as --apps winget:%s\n", id, id)
	return nil
}

func argsNote(c *appcatalog.Custom) string {
	if len(c.Args) == 0 {
		if c.Format == "msi" {
			return " · /qn"
		}
		return " · no switches"
	}
	return " · " + strings.Join(c.Args, " ")
}

func appsAdd(env *Env, args []string) error {
	fs := flag.NewFlagSet("apps add", flag.ContinueOnError)
	id := fs.String("id", "", "picker id (default: derived from the filename)")
	name := fs.String("name", "", "what the picker shows")
	argsFlag := fs.String("args", "", "silent-install switches")
	category := fs.String("category", "", "picker grouping")
	replace := fs.Bool("replace", false, "update an existing id")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("apps add <installer.msi|.exe> [--id x] [--name \"X\"] [--args \"/qn\"]")
	}
	lib, err := env.library()
	if err != nil {
		return err
	}
	prog := &stageProgress{}
	c, err := appcatalog.AddInstaller(env.libraryRoot(), lib, appcatalog.Installer{
		Path: fs.Arg(0), ID: *id, Name: *name,
		Args: *argsFlag, Category: *category, Replace: *replace,
	}, prog.report)
	prog.finish()
	if err != nil {
		return err
	}
	fmt.Printf("Added %s (%s) — %d MiB, sha256 %s\n", c.ID, c.Name, c.Size>>20, c.SHA256[:16])
	fmt.Printf("  runs as: %s\n", c.RunLine())
	fmt.Printf("\nUse it:  dsky install windows-11 --apps %s\n", c.ID)
	return nil
}

func appsSet(env *Env, args []string) error {
	fs := flag.NewFlagSet("apps set", flag.ContinueOnError)
	name := fs.String("name", "", "what the picker shows")
	argsFlag := fs.String("args", "", "silent-install switches")
	category := fs.String("category", "", "picker grouping")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("apps set <id> [--name \"X\"] [--args \"/qn /norestart\"] [--category \"X\"]")
	}
	// Only what was actually passed is changed: an unset flag must not wipe a
	// field, and "" is a legitimate value for --args meaning "no switches".
	changed := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { changed[f.Name] = true })
	if len(changed) == 0 {
		return fmt.Errorf("nothing to change — pass --name, --args or --category")
	}
	c, err := appcatalog.UpdateCustom(env.libraryRoot(), fs.Arg(0), func(c *appcatalog.Custom) {
		if changed["name"] {
			c.Name = *name
		}
		if changed["category"] {
			c.Category = *category
		}
		if changed["args"] {
			c.Args = appcatalog.SplitArgs(*argsFlag)
		}
	})
	if err != nil {
		return err
	}
	fmt.Printf("Updated %s (%s)\n  runs as: %s\n", c.ID, c.Name, c.RunLine())
	return nil
}

func appsRemove(env *Env, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("apps remove <id>")
	}
	c, err := appcatalog.RemoveCustom(env.libraryRoot(), args[0])
	if err != nil {
		return err
	}
	fmt.Printf("Removed %s (%s).\n", c.ID, c.Name)
	fmt.Printf("  %s is still in the library; `dsky gc` reclaims it once nothing refers to it.\n", c.Filename)
	return nil
}

func appsShow(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("apps show <id>")
	}
	a, ok := appcatalog.Get(args[0])
	if !ok {
		return fmt.Errorf("unknown program %q — see `dsky apps`", args[0])
	}
	fmt.Printf("%s — %s\n", a.ID, a.Name)
	fmt.Printf("  category: %s\n", a.Category)
	if a.Custom == nil {
		fmt.Printf("  built in\n")
		if a.Winget != "" {
			fmt.Printf("  winget:   %s\n", a.Winget)
		}
		if u := a.Ubuntu; u != nil {
			switch {
			case u.Apt != "":
				fmt.Printf("  ubuntu:   apt %s\n", u.Apt)
			case u.Snap != "":
				fmt.Printf("  ubuntu:   snap %s\n", u.Snap)
			case u.Flatpak != "":
				fmt.Printf("  ubuntu:   flathub %s\n", u.Flatpak)
			case u.Repo != "":
				fmt.Printf("  ubuntu:   vendor repository (%s)\n", u.Repo)
			}
		}
		return nil
	}
	c := a.Custom
	fmt.Printf("  yours, added %s\n", c.AddedAt.Format("2006-01-02"))
	fmt.Printf("  file:     %s (%d MiB)\n", c.Filename, c.Size>>20)
	fmt.Printf("  sha256:   %s\n", c.SHA256)
	fmt.Printf("  runs as:  %s\n", c.RunLine())
	return nil
}
