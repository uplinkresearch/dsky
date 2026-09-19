package helpers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/uplinkresearch/dsky/internal/fetch"
	"github.com/uplinkresearch/dsky/internal/manifest"

	"github.com/uplinkresearch/dsky/internal/hidewin"
)

// Fido (github.com/pbatard/Fido, GPLv3, by the Rufus author) resolves
// Microsoft's ephemeral consumer-ISO download URLs. It is fetched once as a
// hash-pinned helper and always run as a subprocess.
const (
	fidoVersion = "v1.70"
	fidoURL     = "https://raw.githubusercontent.com/pbatard/Fido/" + fidoVersion + "/Fido.ps1"
	fidoSHA256  = "24c86067fa399d2fd75ef0693a2ec79ca8db162827f808caac03541cbf640c13"
)

// EnsureFido downloads and verifies the pinned Fido script if it is not
// cached yet, returning its path.
func EnsureFido(ctx context.Context, helpersDir string) (string, error) {
	dir := filepath.Join(helpersDir, "fido")
	path := filepath.Join(dir, "Fido-"+fidoVersion+".ps1")
	if sum, err := fetch.SHA256File(path); err == nil {
		if sum == fidoSHA256 {
			return path, nil
		}
		os.Remove(path) // corrupt or tampered — refetch
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	sum, err := fetch.Download(ctx, fidoURL, path, nil)
	if err != nil {
		return "", fmt.Errorf("fetching Fido helper: %w", err)
	}
	if sum != fidoSHA256 {
		os.Remove(path)
		return "", fmt.Errorf("Fido helper hash mismatch: got %s, pinned %s — refusing to run it", sum, fidoSHA256)
	}
	return path, nil
}

// powershellBinary picks the host's PowerShell: Windows PowerShell on
// Windows, pwsh 7+ elsewhere.
//
// Finding a file called pwsh is not enough. Version managers such as mise and
// asdf put a shim on PATH that fails when no version is selected, and running
// one made downloading Windows fail with mise's own error. So each candidate
// is run once, and a PowerShell those tools installed is found in their
// install folders even when the shim is broken.
func powershellBinary(ctx context.Context) (string, error) {
	if runtime.GOOS == "windows" {
		return "powershell", nil
	}
	var candidates []string
	if env := os.Getenv("DSKY_PWSH"); env != "" {
		candidates = append(candidates, env)
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir != "" {
			candidates = append(candidates, filepath.Join(dir, "pwsh"))
		}
	}
	candidates = append(candidates,
		"/opt/homebrew/bin/pwsh", "/usr/local/bin/pwsh", "/usr/bin/pwsh",
		"/opt/microsoft/powershell/7/pwsh", "/usr/local/microsoft/powershell/7/pwsh")
	if home, err := os.UserHomeDir(); err == nil {
		for _, pattern := range []string{
			".local/share/mise/installs/powershell/*/pwsh",
			".asdf/installs/powershell*/*/pwsh",
			".dotnet/tools/pwsh",
		} {
			matches, _ := filepath.Glob(filepath.Join(home, pattern))
			for i := len(matches) - 1; i >= 0; i-- { // newest-looking first
				candidates = append(candidates, matches[i])
			}
		}
	}
	// A pwsh that exists and will not run is worth remembering: telling
	// somebody to install PowerShell when PowerShell is sitting on their PATH
	// sends them round in a circle. What the thing said when it failed is
	// usually the fix -- mise, for one, prints the exact command.
	var brokenPath, brokenSaid string
	seen := map[string]bool{}
	for _, c := range candidates {
		if seen[c] {
			continue
		}
		seen[c] = true
		if st, err := os.Stat(c); err != nil || st.IsDir() {
			continue
		}
		check, cancel := context.WithTimeout(ctx, 20*time.Second)
		out, err := hidewin.Cmd(exec.CommandContext(check, c, "-NoProfile", "-NonInteractive", "-Command", "$PSVersionTable.PSVersion.Major")).Output()
		cancel()
		if major, perr := strconv.Atoi(strings.TrimSpace(string(out))); err == nil && perr == nil && major >= 7 {
			return c, nil
		}
		if brokenPath == "" {
			brokenPath, brokenSaid = c, whyItFailed(err, string(out))
		}
	}
	const orISO = "Or download the ISO in a browser from microsoft.com/software-download " +
		"and choose it under \"Use an ISO you downloaded\" (--iso <file> with dsky install)"
	if brokenPath != "" {
		// Found, and not usable. Do not say "install it".
		said := ""
		if brokenSaid != "" {
			said = fmt.Sprintf(" It said: %s.", brokenSaid)
		}
		return "", fmt.Errorf("fetching Windows on this computer needs PowerShell 7. %s is on this computer "+
			"but does not run.%s Fix that — a version manager usually needs a version chosen, and prints how "+
			"— or install PowerShell 7 properly (https://aka.ms/powershell). %s", brokenPath, said, orISO)
	}
	hint := "install it with your package manager, e.g. mise use -g powershell"
	if runtime.GOOS == "darwin" {
		hint = "brew install powershell"
	}
	return "", fmt.Errorf("fetching Windows on this computer needs PowerShell 7 (%s; https://aka.ms/powershell). "+
		"%s", hint, orISO)
}

// whyItFailed is the first useful line the thing printed, from wherever it
// printed it. A shim's complaint is the operator's instruction: mise answers
// "No version is set for shim: pwsh", and then tells them what to run.
func whyItFailed(runErr error, stdout string) string {
	var text string
	var ee *exec.ExitError
	if errors.As(runErr, &ee) && len(ee.Stderr) > 0 {
		text = string(ee.Stderr)
	}
	if strings.TrimSpace(text) == "" {
		text = stdout
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "mise ERROR "))
		// Version managers print their own version as a trailing line; it is
		// not why anything failed.
		if line == "" || strings.HasPrefix(line, "Run with") || strings.HasPrefix(line, "Version:") {
			continue
		}
		if len(line) > 120 {
			line = line[:120] + "…"
		}
		return line
	}
	return ""
}

var (
	pwshMu      sync.Mutex
	pwshFound   bool
	pwshChecked time.Time
)

// CanFetchWindows reports whether this computer can fetch Windows from
// Microsoft: always on Windows, and elsewhere when a working PowerShell 7 is
// installed. A negative answer is rechecked after a minute, so installing
// PowerShell takes effect without restarting DSKY.
func CanFetchWindows(ctx context.Context) bool {
	if runtime.GOOS == "windows" {
		return true
	}
	pwshMu.Lock()
	defer pwshMu.Unlock()
	if pwshFound || time.Since(pwshChecked) < time.Minute {
		return pwshFound
	}
	_, err := powershellBinary(ctx)
	pwshFound, pwshChecked = err == nil, time.Now()
	return pwshFound
}

// Fido stops on anything but Windows: it takes the Windows version from the
// OS, gives every other platform version 0.0, and then refuses any version at
// or below Windows 7 ("This feature is not available on this platform.").
// Its author did that on purpose in March 2023 (commit 425eb4d, issues #58 and
// #60) so as not to support platforms he does not test, and told anyone who
// wanted Linux to keep their own copy. This is that copy: one line changed, so
// a platform that is not Windows counts as a current one. The download is
// still Microsoft's and still resolved by Fido's own code.
//
// The patch is applied on this machine to the hash-verified original and the
// result is verified against its own pinned hash, so a changed upstream line
// fails loudly instead of running something unreviewed. Moving fidoVersion
// means re-deriving fidoPatchedSHA256 (TestFidoPatch prints it).
const (
	fidoPatchFrom      = "\t$version = 0.0\n"
	fidoPatchTo        = "\t$version = 10.0 # DSKY: not Windows counts as Windows 10+, so Fido runs on macOS and Linux\n"
	fidoPatchedSHA256  = "30ceaf0c0d452a1b9021718a7ac8e5936e8f8e576886e149fe9104223261b18a"
	fidoModifiedNotice = "# Modified by DSKY (github.com/uplinkresearch/dsky) from Fido " + fidoVersion + ":\n" +
		"# Get-Platform-Version treats non-Windows platforms as Windows 10 so the script\n" +
		"# runs under PowerShell 7 on macOS and Linux. Original: " + fidoURL + "\n"
)

// fidoForHost returns the script to run here: Fido as pinned on Windows, and
// DSKY's patched copy everywhere else.
func fidoForHost(original string) (string, error) {
	if runtime.GOOS == "windows" {
		return original, nil
	}
	patched := strings.TrimSuffix(original, ".ps1") + "-dsky.ps1"
	if sum, err := fetch.SHA256File(patched); err == nil && sum == fidoPatchedSHA256 {
		return patched, nil
	}
	b, err := os.ReadFile(original)
	if err != nil {
		return "", err
	}
	out, err := patchFido(b)
	if err != nil {
		return "", err
	}
	tmp := patched + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return "", err
	}
	if sum, err := fetch.SHA256File(tmp); err != nil || sum != fidoPatchedSHA256 {
		os.Remove(tmp)
		return "", fmt.Errorf("patched Fido hash mismatch: got %s, pinned %s — refusing to run it", sum, fidoPatchedSHA256)
	}
	return patched, os.Rename(tmp, patched)
}

func patchFido(b []byte) ([]byte, error) {
	s := string(b)
	if n := strings.Count(s, fidoPatchFrom); n != 1 {
		return nil, fmt.Errorf("Fido %s no longer has the one line DSKY patches to run it off Windows (found %d)", fidoVersion, n)
	}
	s = strings.Replace(s, fidoPatchFrom, fidoPatchTo, 1)
	// The notice goes after the byte-order mark: ahead of it, PowerShell reads
	// the mark as a stray character, the first comment stops being one, and
	// the param block no longer parses.
	bom, rest := "", s
	if strings.HasPrefix(s, "\ufeff") {
		bom, rest = "\ufeff", strings.TrimPrefix(s, "\ufeff")
	}
	return []byte(bom + fidoModifiedNotice + rest), nil
}

// ResolveFidoURL runs Fido -GetUrl for the spec and returns the ephemeral
// Microsoft download URL (valid for roughly 24 hours).
//
// Fido refuses to run anywhere but Windows, so on macOS and Linux the copy
// that runs is patched first; see fidoForHost.
func ResolveFidoURL(ctx context.Context, helpersDir string, spec *manifest.FidoSpec) (string, error) {
	original, err := EnsureFido(ctx, helpersDir)
	if err != nil {
		return "", err
	}
	script, err := fidoForHost(original)
	if err != nil {
		return "", err
	}
	s := manifest.FidoSpec{Win: "11", Release: "Latest", Edition: "Pro", Language: "English", Arch: "x64"}
	if spec != nil {
		if spec.Win != "" {
			s.Win = spec.Win
		}
		if spec.Release != "" {
			s.Release = spec.Release
		}
		if spec.Edition != "" {
			s.Edition = spec.Edition
		}
		if spec.Language != "" {
			s.Language = spec.Language
		}
		if spec.Arch != "" {
			s.Arch = spec.Arch
		}
	}
	// Microsoft URLs stay valid ~24h and their rate limiter ("Sentinel")
	// blocks an IP after only a few link requests — so never ask twice for
	// the same thing while a resolved URL is still fresh.
	cacheKey := fmt.Sprintf("%s|%s|%s|%s|%s", s.Win, s.Release, s.Edition, s.Language, s.Arch)
	cachePath := filepath.Join(helpersDir, "fido", "urlcache.json")
	if url := cachedURL(cachePath, cacheKey); url != "" {
		return url, nil
	}
	// Asking again straight after a refusal only prolongs it, and every click
	// of Set up or Install asked again. For an hour after one, say so without
	// asking; a different network after that gets a fresh try.
	if at, ok := recentRejection(cachePath); ok {
		return "", sentinelError(s.Win, at)
	}
	ps, err := powershellBinary(ctx)
	if err != nil {
		return "", err
	}

	cmd := hidewin.Cmd(exec.CommandContext(ctx, ps,
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-File", script,
		"-Win", s.Win, "-Rel", s.Release, "-Ed", s.Edition,
		"-Lang", s.Language, "-Arch", s.Arch, "-GetUrl",
		// Without this Fido asks Windows for the CPU type (Get-CimInstance,
		// which PowerShell on macOS and Linux lacks) and stops. It only sets
		// Fido's default choice; -Arch decides the download.
		"-PlatformArch", s.Arch))
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if ee, ok := err.(*exec.ExitError); ok {
			detail = strings.TrimSpace(detail + "\n" + strings.TrimSpace(string(ee.Stderr)))
		}
		if strings.Contains(detail, "Sentinel") {
			saveRejection(cachePath, time.Now())
			return "", sentinelError(s.Win, time.Now())
		}
		return "", fmt.Errorf("Fido could not resolve a download URL: %w\n%s", err, detail)
	}
	url := strings.TrimSpace(string(out))
	if !strings.HasPrefix(url, "https://") {
		return "", fmt.Errorf("Fido returned no URL: %s", url)
	}
	saveCachedURL(cachePath, cacheKey, url)
	return url, nil
}

type fidoCacheEntry struct {
	URL       string    `json:"url"`
	FetchedAt time.Time `json:"fetched_at"`
}

// sentinelError explains a refusal from Microsoft's rate limiter in terms of
// what to do in DSKY: the ISO field, not a command a Quick Install user has no
// workspace for.
func sentinelError(win string, at time.Time) error {
	page := "microsoft.com/software-download/windows11"
	if win == "10" {
		page = "microsoft.com/software-download/windows10"
	}
	return fmt.Errorf("Microsoft refused the download link (its rate limiter blocks an address for about a day after a few requests; refused at %s). "+
		"Download the ISO in a browser from %s, then choose it under \"Use an ISO you downloaded\" and try again (--iso <file> with dsky install). "+
		"Or try again tomorrow, or from another network", at.Local().Format("15:04"), page)
}

const rejectionKey = "_sentinel_rejected_at"

func saveRejection(path string, at time.Time) {
	m := map[string]fidoCacheEntry{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	m[rejectionKey] = fidoCacheEntry{FetchedAt: at.UTC()}
	if b, err := json.MarshalIndent(m, "", "  "); err == nil {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		_ = os.WriteFile(path, b, 0o644)
	}
}

func recentRejection(path string) (time.Time, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}
	var m map[string]fidoCacheEntry
	if json.Unmarshal(b, &m) != nil {
		return time.Time{}, false
	}
	e, ok := m[rejectionKey]
	if !ok || time.Since(e.FetchedAt) > time.Hour {
		return time.Time{}, false
	}
	return e.FetchedAt, true
}

// cachedURL returns a still-fresh previously resolved URL for key, if any.
func cachedURL(path, key string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var m map[string]fidoCacheEntry
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	e, ok := m[key]
	if !ok || time.Since(e.FetchedAt) > 20*time.Hour {
		return ""
	}
	return e.URL
}

func saveCachedURL(path, key, url string) {
	m := map[string]fidoCacheEntry{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	m[key] = fidoCacheEntry{URL: url, FetchedAt: time.Now().UTC()}
	if b, err := json.MarshalIndent(m, "", "  "); err == nil {
		_ = os.WriteFile(path, b, 0o644)
	}
}
