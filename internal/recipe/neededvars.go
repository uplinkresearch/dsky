package recipe

import (
	"sort"
	"strings"
)

// What a recipe cannot be built without.
//
// A recipe keeps passwords out of itself on purpose: it lives in a workspace,
// goes into version control, and outlives every machine it was ever used for.
// So it says "${var:admin_password}" and the value arrives at build time from
// vars.local.yaml, which is gitignored, or from --var.
//
// The failure that follows is two different shapes, and only one of them was
// caught. A var with no value anywhere stops the build and names itself, which
// is how a missing Wi-Fi passphrase behaves. A var defined as an empty string
// does not: it expands to nothing and the build carries on. The scaffolded
// workspace.yaml ships `admin_password: ""` as a default, so a recipe saved
// from the Install dialog with an administrator password -- which the recipe
// deliberately does not store -- built a machine whose administrator had no
// password at all, and said nothing.
//
// The two cases are not the same thing and must not behave the same way:
//
//	admin_password: ""                      somebody means no password
//	admin_password: "${var:admin_password}"  somebody means there is one, elsewhere
//
// The second resolving to empty means the value went missing on the way, not
// that the password is empty. So for a value that is a password, an empty one
// counts as missing.

// passwordSuffix marks a var whose value is a password somebody typed. Suffix
// rather than a list, so domain_password and anything added later are covered
// without anybody remembering to add them.
//
// admin_password_hash deliberately does not match. It is not typed by anybody:
// DSKY derives it from the password, and the scaffolded vars.local.yaml ships
// one. A rule of "any name containing password" would sweep it in, and an
// empty hash means something different from an empty password.
const passwordSuffix = "_password"

// IsPasswordVar reports whether a value held under this name is a password.
func IsPasswordVar(name string) bool { return strings.HasSuffix(name, passwordSuffix) }

// UnresolvedVars lists the ${var:...} names in s that will not produce a
// usable value. key is the name the value is held under, which is what decides
// whether an empty string counts as missing.
func UnresolvedVars(key, s string, vars map[string]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range varRe.FindAllStringSubmatch(s, -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		v, defined := vars[name]
		if defined && !(IsPasswordVar(key) && strings.TrimSpace(v) == "") {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// NeededVars lists every var a build of r will ask the workspace for and not
// get, sorted and without repeats. An empty result means the recipe has
// everything it needs.
func NeededVars(r *Recipe, vars map[string]string) []string {
	missing := map[string]bool{}
	add := func(key, value string) {
		for _, name := range UnresolvedVars(key, value, vars) {
			missing[name] = true
		}
	}
	if w := r.Windows; w != nil {
		if w.Unattend != nil {
			for _, k := range sortedKeys(w.Unattend.Vars) {
				add(k, w.Unattend.Vars[k])
			}
		}
		// The passphrase and the join credential are held in their own
		// fields rather than in a map, so they are named here by what they
		// are. Getting this wrong is silent in the worst way: a wireless
		// profile with an empty key joins nothing, and a join with an empty
		// password is an install that stops at a prompt nobody is watching.
		if w.WiFi != nil {
			add("wifi_password", w.WiFi.Password)
			add("wifi_ssid", w.WiFi.SSID)
		}
		if w.Domain != nil {
			add("domain_password", w.Domain.Password)
			add("domain_username", w.Domain.Username)
			add("domain_join", w.Domain.Join)
			add("domain_ou", w.Domain.OU)
		}
	}
	if l := r.Linux; l != nil {
		if l.Autoinstall != nil {
			for _, k := range sortedKeys(l.Autoinstall.Vars) {
				add(k, l.Autoinstall.Vars[k])
			}
		}
		if l.Kickstart != nil {
			for _, k := range sortedKeys(l.Kickstart.Vars) {
				add(k, l.Kickstart.Vars[k])
			}
		}
	}
	out := make([]string, 0, len(missing))
	for name := range missing {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// NeededVarsError is the sentence a build and a save both say about them, so
// the two do not drift into describing the same problem differently.
func NeededVarsError(names []string) string {
	if len(names) == 0 {
		return ""
	}
	what := "a value"
	if len(names) > 1 {
		what = "values"
	}
	return "this recipe needs " + what + " it does not carry: " + strings.Join(names, ", ") +
		". Passwords are deliberately kept out of a recipe, because a recipe goes into version " +
		"control and outlives the machine. Put them in vars.local.yaml beside the recipe, which is " +
		"gitignored, or pass them with --var"
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
