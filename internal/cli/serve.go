package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uplinkresearch/dsky/internal/appcatalog"
	"github.com/uplinkresearch/dsky/internal/appconfig"
	"github.com/uplinkresearch/dsky/internal/jobs"
	"github.com/uplinkresearch/dsky/internal/library"
	"github.com/uplinkresearch/dsky/internal/oscatalog"
	"github.com/uplinkresearch/dsky/internal/selfupdate"
	"github.com/uplinkresearch/dsky/internal/webui"
)

// idleGrace is how long the portal waits after the last page closes. Long
// enough that a reload (which drops the event stream and reopens it) is not
// mistaken for leaving, short enough that a forgotten tab does not leave a
// server running all afternoon.
const idleGrace = 45 * time.Second

func cmdServe(ctx context.Context, env *Env, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	port := fs.Int("port", 8931, "loopback port (auto-increments if busy)")
	open := fs.Bool("open", false, "open the page in the default browser")
	stay := fs.Bool("keep-alive", false, "keep serving after the last page closes")
	newInst := fs.Bool("new", false, "start another portal even if one is running")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	// Installs up to v0.7.0 wrote a launcher that runs `serve --open`, from
	// before macOS and Linux had a window. Updating replaces the binary, not
	// the launcher, so without this every existing install would keep opening
	// a browser tab. Rewritten once, and this launch gets the window too.
	if *open && len(args) == 1 && upgradeLauncher() {
		return AppMain()
	}
	lib, err := env.library()
	if err != nil {
		return err
	}
	if !*newInst {
		if inst, ok := liveInstance(lib.Root); ok {
			fmt.Printf("A portal is already running.\n\nOpen:  %s\n", inst.url())
			if *open {
				openBrowser(inst.url())
			}
			fmt.Println("\n(`dsky serve --new` starts a second one anyway.)")
			return nil
		}
	}
	idle := idleGrace
	if *stay {
		idle = 0
	}
	// A portal started with --new is an extra, not a replacement, so it does
	// not take the record. The record is how the next launch finds the one
	// portal to raise, and a second instance claiming it means the app hands
	// its window to whatever was started alongside — a development build in a
	// terminal, say, which then answers for the installed one: its Update
	// button replaces that build's binary rather than the one on disk, and it
	// has no window to restart. The symptom is an update that reports success
	// and changes nothing.
	claim := !*newInst
	url, done, err := startServer(ctx, lib, env.Vars, *port, env.WorkspaceDir, *open, true, claim, idle, nil)
	if err != nil {
		return err
	}
	fmt.Printf("Open:  %s\n", url)
	fmt.Println("The token in the URL is this session's key — the page needs it.")
	if *stay {
		fmt.Println("Ctrl-C (or Quit in the page) stops it.")
	} else {
		// Said plainly, because a server that stops on its own is surprising
		// if you were not told — and this is the behaviour people expect from
		// something they closed.
		fmt.Println("It stops on its own shortly after you close the page. Anything still")
		fmt.Println("running — a flash, a build — keeps it open until it finishes.")
	}
	<-done
	return nil
}

// AppMain is the entry point for the windowless launcher (dsky-app): open
// the portal in the browser and serve until the page's Quit button or a
// signal stops it.
func AppMain() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	lib, err := library.Open(library.DefaultRoot())
	if err != nil {
		return err
	}
	// A process started by the updater is racing the one it replaced. Give that
	// one a moment to let go of its port, or the check below finds it still
	// listening, decides a portal is already running, and hands the window
	// straight back to the build we have just replaced.
	if selfupdate.WasRestarted() {
		waitForPredecessor(lib.Root)
	}
	// Launching the app when it is already running should bring back the
	// portal you already have, not start a second one behind it.
	if inst, ok := liveInstance(lib.Root); ok {
		// Its own window, raised. Failing that the portal is running without
		// one — a `serve` in a terminal — so give it one, or a browser.
		if focusWindow(appTitle) {
			return nil
		}
		if !showWindow(inst.url(), appTitle) {
			openBrowser(inst.url())
		}
		return nil
	}
	// open=false: the window below is the way in. Only the browser fallback
	// needs one opened for it.
	url, done, err := startServer(ctx, lib, map[string]string{}, 8931, ".", false, false, true, idleGrace,
		func() { restartAfterUpdate(stop) })
	if err != nil {
		return err
	}
	// The other direction: when the server stops first — Quit in the page, or
	// the idle timeout — the window goes too, rather than staying on screen
	// serving nothing.
	//
	// Not during an update restart, which stops the server on purpose and
	// relaunches a moment later: closing the window there would end this
	// process before the relaunch, and the app would never come back.
	go func() {
		<-done
		if !restarting.Load() {
			closeWindow()
		}
	}()
	if showWindow(url, appTitle) {
		// The window is the app. Closing it quits, which is the gesture
		// everybody already uses and previously did nothing.
		//
		// A flash in progress is not lost: writes run in a separate elevated
		// worker, so the stick is finished and verified even though the
		// portal that started it has gone.
		stop()
		// Give the server its moment to unwind and delete the record of itself.
		// Returning straight after stop() races that, and losing the race
		// strands a record pointing at a dead port — recoverable, since the
		// next launch probes rather than trusts it, but it costs that launch a
		// timeout for no reason. Bounded, so a wedged server cannot leave the
		// process hanging around invisibly after its window has gone.
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		waitForRelaunch()
		return nil
	}
	// No window to be had — fall back to the browser, where the idle timeout
	// is what eventually stops the server.
	openBrowser(url)
	<-done
	waitForRelaunch()
	return nil
}

// waitForRelaunch holds this process open while an update restart is under
// way, until the new build has been started.
//
// Returning from AppMain ends the process, and an update restart begins by
// stopping the server — which is exactly what the callers above wait for. So
// without this the process exited in the gap between stopping and relaunching,
// and the update installed with nothing reopening. In a browser tab that was
// every time; the window paths survived only because the window happened to
// outlast the gap.
func waitForRelaunch() {
	if restarting.Load() {
		<-relaunched
	}
}

// restarting is set while restartAfterUpdate is replacing this process, so the
// server stopping is not taken as a reason to close the window or to exit.
// relaunched closes once the new build has been started (or failed to start).
var (
	restarting   atomic.Bool
	relaunched   = make(chan struct{})
	relaunchOnce sync.Once
)

// appTitle is what the window is called in the taskbar and the title bar.
const appTitle = "DSKY"

// waitForPredecessor blocks until the portal this process is replacing has let
// go, or until waiting stops being worth it.
//
// Bounded on purpose: if the old process is wedged rather than exiting, the
// right outcome is a portal on the next free port, not an app that never opens
// because its predecessor would not die.
func waitForPredecessor(libRoot string) {
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := liveInstance(libRoot); !ok {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// restartAfterUpdate starts the freshly installed build and stops this one.
//
// The order matters. The server is stopped first so the port is free and the
// instance record is gone before the replacement looks for them; the new
// process also waits, because "stopped" and "the socket is released" are not
// the same instant.
func restartAfterUpdate(stop func()) {
	restarting.Store(true)
	// Stop before starting, not after. The replacement wants this port and
	// this instance record, and a new process that finds the old one still
	// listening concludes a portal is already running and hands the window
	// straight back to the build it was meant to replace.
	//
	// Doing it in this order also means the new build needs no cooperation,
	// which matters because the version doing the restarting is always older
	// than the version being restarted into.
	stop()
	// Cancelling the context and the socket actually closing are not the same
	// instant. waitForPredecessor on the far side covers the rest.
	time.Sleep(600 * time.Millisecond)
	// With the same arguments: `dsky app` restarted as a bare `dsky` would
	// print the usage and exit, leaving no app at all. dsky-app has none.
	_ = selfupdate.Relaunch(os.Args[1:]...)
	relaunchOnce.Do(func() { close(relaunched) })
	// The window, not the server, is what holds this process open — stopping
	// the server alone leaves an app on screen serving nothing. Closed even if
	// the relaunch failed, because the alternative is a window whose portal is
	// already dead; the binary on disk is the new one either way, so opening it
	// again gets what was asked for.
	closeWindow()
	// And then leave, whether or not the window took the hint.
	//
	// Asking a webview to close is a request to a message loop this goroutine
	// does not own, and observed behaviour is that it can be ignored: the
	// replacement started and took the port while the old build sat there with
	// its window open, serving nothing, looking like the update had produced a
	// second copy of the app. There is nothing left to wind down by this point
	// — the server is stopped and the successor is already running — so the
	// only question is whether this process leaves promptly or not at all.
	time.AfterFunc(2*time.Second, func() { os.Exit(0) })
}

// startServer binds a free port at or after base, wires the workspace
// (explicit -w if present, else the last-opened one), optionally opens the
// browser, and runs the server in the background. It returns the tokened URL
// and a channel that closes when the server stops.
//
// claim says whether this portal is the one a later launch should be sent to.
// Only a deliberate second instance (`serve --new`) says no.
func startServer(ctx context.Context, lib *library.Library, vars map[string]string, base int, wsDir string, open, announce, claim bool, idle time.Duration, onUpdated func()) (string, <-chan struct{}, error) {
	var tok [16]byte
	if _, err := rand.Read(tok[:]); err != nil {
		return "", nil, err
	}
	cfg := appconfig.Load()
	// The operator's own installers, and the OS list last verified. Loaded
	// here rather than in the command path because the app people click
	// (AppMain) never goes through it: installers added in the window were
	// saved to disk and then invisible the next time it opened, and every
	// recipe that used one built without it.
	oscatalog.LoadCached(lib.Root)
	// ...and then go and ask for a newer one. The CLI refreshes before any
	// command that shows the catalog; the window never goes through that
	// path, so until now it offered whatever a CLI run happened to cache
	// last -- and on a machine where somebody only ever opens the app, that
	// is whatever shipped with the build. A newly published OS was invisible
	// with nothing on screen to say so.
	go refreshCatalogWhileOpen(ctx, lib.Root)
	if err := appcatalog.LoadCustom(lib.Root); err != nil {
		fmt.Fprintln(os.Stderr, "warning: your added programs could not be read:", err)
	}
	s := &webui.Server{
		Lib: lib, CLIVars: vars, Token: hex.EncodeToString(tok[:]),
		Reg: jobs.NewRegistry(), Cfg: cfg, IdleTimeout: idle,
		OnUpdated: onUpdated,
	}
	if err := s.SetWorkspaceDir(wsDir); err != nil {
		if last := cfg.Current(); last != "" {
			_ = s.SetWorkspaceDir(last)
		}
	}

	ln, port, err := listen(base)
	if err != nil {
		return "", nil, err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/#t=%s", port, s.Token)
	if announce {
		if name := s.WorkspaceName(); name != "" {
			fmt.Printf("DSKY — workspace %q\n", name)
		} else {
			fmt.Println("DSKY — no workspace open yet (pick one in the page)")
		}
	}
	// Recorded before serving, so a second launch a moment later finds it.
	inst := instance{Port: port, Token: s.Token, PID: os.Getpid(),
		Started: time.Now().Format(time.RFC3339)}
	if claim {
		writeInstance(lib.Root, inst)
	}

	done := make(chan struct{})
	go func() {
		_ = webui.Serve(ctx, ln, s)
		// Safe either way: clearInstance only removes a record that is ours,
		// so an unclaimed instance cannot take the real portal's down with it.
		clearInstance(lib.Root, inst)
		close(done)
	}()
	if open {
		go func() { time.Sleep(300 * time.Millisecond); openBrowser(url) }()
	}
	return url, done, nil
}

// listen binds 127.0.0.1 at base, then base+1..base+19 if busy.
func listen(base int) (net.Listener, int, error) {
	for p := base; p < base+20; p++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			return ln, p, nil
		}
	}
	return nil, 0, fmt.Errorf("no free loopback port in %d..%d", base, base+20)
}

// openBrowser launches the platform's default browser; failures are silent.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
