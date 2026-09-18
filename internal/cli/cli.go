// Package cli implements the dsky command line.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
	"github.com/uplinkresearch/dsky/internal/buildinfo"
	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/oscatalog"
	"github.com/uplinkresearch/dsky/internal/selfupdate"
	"github.com/uplinkresearch/dsky/internal/workspace"
)

const usage = `DSKY — build bootable installation USB media from recipes.

Usage: dsky <recipe>            the whole thing: pull sources, build, flash the attached stick, verify
       dsky <file.iso> [device]  write any ISO or disk image you already have
       dsky                     same, in a workspace with a single recipe
       dsky <command> [args]

One-shot
  go <recipe|image> [device]  pull sources, build, flash, verify (--yes, --build-only)

Quick install (no workspace needed)
  catalog                   list the operating systems on offer
  install <os-id> [device]  build + flash an OS from the catalog
                            (--edition, --account local|oobe, --debloat, --bypass-checks,
                             --drivers to detect this machine and stage its drivers,
                             --drivers-for "dell:OptiPlex 7010" for another model (repeatable),
                             --apps chrome,7zip,... to install programs at first boot
                               (set:business for a starter set, winget:Publisher.Package for any winget package),
                             --domain-blob <file> to join a domain offline,
                             --iso <file> to use an ISO you downloaded yourself)
  detect                    what this computer is, and the drivers it needs

Programs
  apps                      programs --apps can install
  apps add <installer>      add your own .msi/.exe (--id, --name, --args)
  apps winget <id>          check winget has a package (for --apps winget:<id>)
  apps set|remove|show <id>   edit, forget, or inspect one of yours

Replacing a PC (see docs/plan-migrate.md)
  migrate validate <file>   check a migration manifest and summarise it
  migrate report <file>     render it as a page to read (--out, --os)
  migrate schema            the published JSON Schema for the manifest

Workspace
  init --org <name> [dir]   scaffold a new org workspace
  recipes list              recipes in the workspace
  recipes lint              production-lesson checks + repo hygiene

Library (machine-local, multi-GB safe)
  sources list              manifests and their library status
  sources pull <id>         download a pinned source (--pin-tofu to trust-on-first-use;
                            provider: fido manifests fetch official Windows ISOs directly)
  sources import <id> <file>  add a manually-downloaded file (e.g. Windows ISO)
  gc                        drop unreferenced blobs and tmp files

Drivers
  drivers search <dell|lenovo|hp|framework> "<model>"   find the vendor's driver pack for a model (--add)
  drivers models <dell|lenovo|hp|framework>   list every model the vendor has drivers for
  drivers search mscatalog "<hardware-id>"     find a driver in the Microsoft Update Catalog (--add)
  drivers resolve <recipe>  fetch packs for every windows.hardware entry (compose does this itself)
  drivers inspect <pack>    what a dir/.zip/.cab/.inf covers (class, versions, hardware IDs)
  drivers add --id <n> <pack>  stage a pack you already have + recipe snippet
  drivers scan              list this machine's devices that still need drivers (Windows)

Building and flashing
  build <recipe>            compose a bootable artifact into the library
  devices                   list candidate USB targets
  flash <recipe|image> <device> [<device> …]  write and verify sticks, all at once
                            (--all for every attached stick; one elevation for the batch)
                            image = any .iso/.img you have, compressed or not —
                            it does not need to be in the catalog
  clone <device>            read a working stick into the library as a master image
                            (--to <device> … , or --to all, to write it straight to blanks)

Disk utilities (removable USB media only)
  disks                     every disk, with what is actually on it
  disks inspect <device>    partition table, volumes, and why the OS may not mount it
  disks prepare <device>    erase and lay down one full-size volume
                            (--fs exfat|fat32|ntfs, --scheme gpt|mbr, --label NAME)

Pick an interface
  tui                       full-screen terminal wizard (pick OS, options, stick)
  app                       the web UI in its own window; closing the window quits
  serve [--port 8931]       local web UI (recipes, devices, build, flash, live progress)

Other
  update [--check]          replace this binary with the newest release
  uninstall [--purge]       remove the installed program (--purge also deletes
                            the downloaded-image library; workspaces are never touched)
  doctor                    check this host's tooling and configuration
  disk-probe <disk> <size>  ask Windows which writes a stick accepts (diagnosis)
  version                   print version

Global flags (before or after the command):
  -w <dir>      workspace directory (default: current directory, searching upward)
  --var k=v     set/override a template var (repeatable)
  --library <dir>  override the library root
`

// Env carries resolved global state into commands.
type Env struct {
	WorkspaceDir string
	LibraryRoot  string
	Vars         map[string]string
}

func (e *Env) libraryRoot() string {
	if e.LibraryRoot != "" {
		return e.LibraryRoot
	}
	return library.DefaultRoot()
}

func (e *Env) library() (*library.Library, error) {
	return library.Open(e.libraryRoot())
}

// refreshCatalog pulls the published OS list before a command that shows or
// uses it. Kept short and non-fatal: a bench with no network still works
// from the cached index, or the one compiled in.
func refreshCatalog(ctx context.Context, env *Env) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	_ = oscatalog.Refresh(ctx, env.libraryRoot())
}

func (e *Env) workspace() (*workspace.Workspace, error) {
	dir, err := workspace.Find(e.WorkspaceDir)
	if err != nil {
		return nil, err
	}
	return workspace.Load(dir)
}

// Main is the process entry point; returns the exit code.
func Main(args []string) int {
	env := &Env{WorkspaceDir: ".", Vars: map[string]string{}}

	// Extract global flags anywhere on the line; leave the rest.
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-w" || a == "--workspace":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: -w needs a directory")
				return 2
			}
			i++
			env.WorkspaceDir = args[i]
		case a == "--library":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "error: --library needs a directory")
				return 2
			}
			i++
			env.LibraryRoot = args[i]
		case a == "--var":
			if i+1 >= len(args) || !strings.Contains(args[i+1], "=") {
				fmt.Fprintln(os.Stderr, "error: --var needs k=v")
				return 2
			}
			i++
			kv := strings.SplitN(args[i], "=", 2)
			env.Vars[kv[0]] = kv[1]
		default:
			rest = append(rest, a)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Windows cannot delete the previous binary while it is still running, so
	// an update leaves it behind for the next run to clear.
	selfupdate.CleanupOld()

	// Adopt the last verified catalog index. No network, so every command
	// starts with a current OS list even offline; commands that actually use
	// the catalog refresh it themselves.
	oscatalog.LoadCached(env.libraryRoot())

	// The operator's own installers, which every picker then shows alongside
	// the built-in programs. A broken store is worth saying out loud rather
	// than quietly presenting a short list as if it were complete.
	if err := appcatalog.LoadCustom(env.libraryRoot()); err != nil {
		fmt.Fprintln(os.Stderr, "warning: your added programs could not be read:", err)
	}

	// "compose this": no arguments in a one-recipe workspace runs it.
	if len(rest) == 0 {
		if id := soleRecipe(env); id != "" {
			rest = []string{id}
		} else {
			fmt.Print(usage)
			return 2
		}
	}

	cmd, cmdArgs := rest[0], rest[1:]
	var err error
	switch cmd {
	case "version", "--version":
		fmt.Println("dsky", buildinfo.Version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	case "go":
		err = cmdGo(ctx, env, cmdArgs)
	case "init":
		err = cmdInit(cmdArgs)
	case "disk-probe":
		err = cmdDiskProbe(cmdArgs)
	case "doctor":
		err = cmdDoctor(ctx, env)
	case "update":
		err = cmdUpdate(ctx, env, cmdArgs)
	case "uninstall":
		err = cmdUninstall(ctx, env, cmdArgs)
	case "devices":
		err = cmdDevices(ctx)
	case "disks":
		err = cmdDisks(ctx, env, cmdArgs)
	case "sources":
		err = cmdSources(ctx, env, cmdArgs)
	case "recipes":
		err = cmdRecipes(env, cmdArgs)
	case "catalog":
		err = cmdCatalog(ctx, env, cmdArgs)
	case "apps":
		err = cmdApps(ctx, env, cmdArgs)
	case "install":
		err = cmdInstall(ctx, env, cmdArgs)
	case "detect":
		err = cmdDetect(ctx, env, cmdArgs)
	case "drivers":
		err = cmdDrivers(ctx, env, cmdArgs)
	case "migrate":
		err = cmdMigrate(ctx, env, cmdArgs)
	case "build":
		err = cmdBuild(ctx, env, cmdArgs)
	case "flash":
		err = cmdFlash(ctx, env, cmdArgs)
	case "clone", "capture": // capture was the old name for this
		err = cmdClone(ctx, env, cmdArgs)
	case "flash-worker":
		return cmdFlashWorker(ctx, cmdArgs)
	case "gc":
		err = cmdGC(env)
	case "serve":
		err = cmdServe(ctx, env, cmdArgs)
	case "app":
		// What dsky-app is on Windows: the portal in its own window, quitting
		// when the window closes. It is what the app-drawer launcher runs.
		err = AppMain()
	case "tui":
		err = cmdTUI(ctx, env, cmdArgs)
	default:
		// A recipe id (or artifact path) as the first word is the one-shot
		// pipeline: `compose nuc-win11`.
		if isRecipeOrArtifact(env, cmd) {
			err = cmdGo(ctx, env, rest)
			break
		}
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		// A few commands distinguish degrees of not-quite-working: a scan
		// that read most of a machine is worth a different exit code from
		// one that could not run at all, so that a script can tell them
		// apart without reading the words.
		var coded exitError
		if errors.As(err, &coded) {
			return coded.code
		}
		return 1
	}
	return 0
}

// exitError is an error that also says what to exit with.
type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string { return e.err.Error() }
func (e exitError) Unwrap() error { return e.err }

// soleRecipe returns the id of the workspace's only recipe, or "".
func soleRecipe(env *Env) string {
	ws, err := env.workspace()
	if err != nil {
		return ""
	}
	rs, err := ws.Recipes()
	if err != nil || len(rs) != 1 {
		return ""
	}
	return rs[0].ID
}

func isRecipeOrArtifact(env *Env, word string) bool {
	if looksLikeImagePath(word) {
		return true
	}
	ws, err := env.workspace()
	if err != nil {
		return false
	}
	_, err = ws.Recipe(word)
	return err == nil
}

// stageProgress lives in progress.go.
