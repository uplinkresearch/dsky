package migrate

import (
	"fmt"
	"strings"
)

// The settings allowlist is short on purpose, and every entry has to earn its
// place with three things: a place to read it on Windows 10, a way to set it
// on Windows 11, and a check that the two are the same setting. Windows moves
// these between feature updates -- the taskbar search box already means
// something different on 11 than it did on 10 -- so a list that grew by
// guessing would quietly set the wrong things on somebody's new PC.
//
// Everything outside this list is not migrated. Settings the scanner reads
// but cannot apply (the default browser) are still captured, because the
// report saying "set this again by hand" is the whole value of having read it.

// Setting keys. Stable identifiers: they appear in manifests, reports and the
// verify diff, so they outlive whatever registry path is behind them.
const (
	KeyPowerPlan      = "power.plan"
	KeySleepAC        = "power.sleep_ac_minutes"
	KeySleepDC        = "power.sleep_dc_minutes"
	KeyTimezone       = "system.timezone"
	KeyLocale         = "system.locale"
	KeyRegion         = "system.region"
	KeyShowExtensions = "explorer.show_extensions"
	KeyShowHidden     = "explorer.show_hidden"
	KeyTaskbarSearch  = "taskbar.search_mode"
	KeyDefaultBrowser = "defaults.browser"
)

// AllowedSetting is one entry: where it is read, and how it is set again.
// Apply returns an empty method for a setting that cannot be applied, which
// is how "captured for the report only" is expressed.
type AllowedSetting struct {
	CapturedFrom string
	Apply        func(value any) ApplyMethod
	Why          string // why it is worth migrating, for the plan doc and review
}

const (
	advanced = `HKCU\SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\Advanced`
	search   = `HKCU\SOFTWARE\Microsoft\Windows\CurrentVersion\Search`
)

// Allowlist is the whole of what DSKY migrates.
var Allowlist = map[string]AllowedSetting{
	KeyPowerPlan: {
		CapturedFrom: "powercfg /getactivescheme",
		Why:          "a machine set to High performance was set that way for a reason",
		Apply: func(v any) ApplyMethod {
			// The GUID is what powercfg takes; the plan's name differs by
			// language, so the name is only in the report.
			if g := guidOf(v); g != "" {
				return ApplyMethod{Method: ApplyPowerShell, Ref: "powercfg /setactive " + g}
			}
			return ApplyMethod{}
		},
	},
	KeySleepAC: {
		CapturedFrom: "powercfg /q (standby timeout, plugged in)",
		Why:          "a PC at a front desk that sleeps mid-appointment is a support call",
		Apply: func(v any) ApplyMethod {
			return ApplyMethod{Method: ApplyPowerShell, Ref: fmt.Sprintf("powercfg /change standby-timeout-ac %d", intOf(v))}
		},
	},
	KeySleepDC: {
		CapturedFrom: "powercfg /q (standby timeout, on battery)",
		Why:          "same, for laptops",
		Apply: func(v any) ApplyMethod {
			return ApplyMethod{Method: ApplyPowerShell, Ref: fmt.Sprintf("powercfg /change standby-timeout-dc %d", intOf(v))}
		},
	},
	KeyTimezone: {
		CapturedFrom: "Get-TimeZone",
		Why:          "a new machine defaults to the installer's time zone, not the office's",
		Apply: func(v any) ApplyMethod {
			if s := stringOf(v); s != "" {
				return ApplyMethod{Method: ApplyPowerShell, Ref: fmt.Sprintf("Set-TimeZone -Id '%s'", psQuote(s))}
			}
			return ApplyMethod{}
		},
	},
	KeyLocale: {
		CapturedFrom: "Get-WinSystemLocale",
		Why:          "date and number formats people read all day",
		Apply: func(v any) ApplyMethod {
			if s := stringOf(v); s != "" {
				return ApplyMethod{Method: ApplyPowerShell, Ref: fmt.Sprintf("Set-WinSystemLocale -SystemLocale '%s'", psQuote(s))}
			}
			return ApplyMethod{}
		},
	},
	KeyRegion: {
		CapturedFrom: "Get-WinHomeLocation",
		Why:          "regional content and formats",
		Apply: func(v any) ApplyMethod {
			if n := intOf(v); n > 0 {
				return ApplyMethod{Method: ApplyPowerShell, Ref: fmt.Sprintf("Set-WinHomeLocation -GeoId %d", n)}
			}
			return ApplyMethod{}
		},
	},
	KeyShowExtensions: {
		CapturedFrom: advanced + `\HideFileExt`,
		Why:          "anybody who turned file extensions on will turn them on again within the hour",
		Apply: func(v any) ApplyMethod {
			// Captured as "extensions are shown", which is HideFileExt
			// inverted -- the key name is a trap worth hiding here rather
			// than in three other places.
			hide := 1
			if intOf(v) == 1 {
				hide = 0
			}
			return ApplyMethod{Method: ApplyRegistry, Ref: fmt.Sprintf(`%s\HideFileExt=%d`, advanced, hide)}
		},
	},
	KeyShowHidden: {
		CapturedFrom: advanced + `\Hidden`,
		Why:          "same reason",
		Apply: func(v any) ApplyMethod {
			shown := 2
			if intOf(v) == 1 {
				shown = 1
			}
			return ApplyMethod{Method: ApplyRegistry, Ref: fmt.Sprintf(`%s\Hidden=%d`, advanced, shown)}
		},
	},
	KeyTaskbarSearch: {
		CapturedFrom: search + `\SearchboxTaskbarMode`,
		Why:          "the search box eats taskbar room; people who hid it want it hidden",
		Apply: func(v any) ApplyMethod {
			// Windows 10: 0 hidden, 1 icon, 2 box. Windows 11: 0 hidden,
			// 1 icon, 2 icon+label, 3 box. A 10 machine set to "box" maps to
			// the icon on 11, which is the nearest thing that does not eat
			// the middle of the taskbar.
			mode := intOf(v)
			if mode >= 2 {
				mode = 1
			}
			return ApplyMethod{Method: ApplyRegistry, Ref: fmt.Sprintf(`%s\SearchboxTaskbarMode=%d`, search, mode)}
		},
	},
	KeyDefaultBrowser: {
		CapturedFrom: `HKCU\SOFTWARE\Microsoft\Windows\Shell\Associations\UrlAssociations\http\UserChoice`,
		Why:          "worth knowing, and it is the first thing a user notices",
		// Not settable on Windows 11 without forging the hash Windows writes
		// with it, which is the sort of trick that breaks on a Tuesday. The
		// report says to set it by hand.
		Apply: nil,
	},
}

// AllowlistKeys is every setting DSKY migrates, sorted, for the scanner and
// the documentation.
func AllowlistKeys() []string {
	keys := make([]string, 0, len(Allowlist))
	for k := range Allowlist {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}

func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

func intOf(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case uint64:
		return int(x)
	case float64:
		return int(x)
	case string:
		var n int
		fmt.Sscanf(strings.TrimSpace(x), "%d", &n)
		return n
	}
	return 0
}

func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// guidOf picks the GUID out of a power-plan reading, which powercfg prints as
// "Power Scheme GUID: 381b4222-... (Balanced)".
func guidOf(v any) string {
	s := stringOf(v)
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '(' || r == ')' || r == ':' }) {
		if len(f) == 36 && strings.Count(f, "-") == 4 {
			return f
		}
	}
	return ""
}

// psQuote escapes a value for a single-quoted PowerShell string.
func psQuote(s string) string { return strings.ReplaceAll(s, "'", "''") }
