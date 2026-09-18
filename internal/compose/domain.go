package compose

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/uplinkresearch/dsky/internal/recipe"
)

// Domain join, both ways.
//
// The recipe says what to do; the unattend template says how, because that is
// where every other Windows setting already lives and an operator editing
// their own template should not have to touch Go to change it. So this fills
// in a handful of template vars and then checks the rendered result actually
// contains the join.
//
// That check is the important part. A workspace scaffolded before this feature
// has a template with no domain block in it, and rendering it would quietly
// drop the setting: Setup would carry on, the machine would install perfectly,
// and it would be in a workgroup. Nobody inspects a freshly imaged machine for
// domain membership — they find out when a domain login fails, days later and
// somewhere else.

// Markers the rendered unattend must contain for each path. They are the
// elements Windows actually acts on, not comments, so a template cannot
// satisfy the check without really emitting the join.
const (
	markerCredentialed = "<JoinDomain>"
	markerOffline      = "<AccountData>"
)

// domainVars adds the template variables for a domain join.
//
// Values are expanded against the workspace vars first, so a password written
// as ${var:domain_password} is resolved from vars.local.yaml — and a missing
// one fails the build here rather than writing a literal placeholder onto
// media that then silently fails to join.
func domainVars(uvars map[string]string, wsDir string, d *recipe.DomainSpec, base map[string]string, resolve func(ref string) (string, error)) error {
	// Always defined, even when there is no join: templates render with
	// missingkey=error, so an undefined domain_mode would fail every build
	// that used a template carrying the domain block — including recipes that
	// want nothing to do with domains.
	uvars["domain_mode"] = ""
	if !d.Enabled() {
		return nil
	}
	if d.Offline() {
		blob, err := odjBlob(wsDir, d, resolve)
		if err != nil {
			return err
		}
		uvars["domain_mode"] = "offline"
		uvars["domain_odj_blob"] = blob
		return nil
	}

	expand := func(field, v string) (string, error) {
		out, err := recipe.ExpandVars(v, base)
		if err != nil {
			return "", fmt.Errorf("windows.domain.%s: %w", field, err)
		}
		return out, nil
	}
	join, err := expand("join", d.Join)
	if err != nil {
		return err
	}
	user, err := expand("username", d.Username)
	if err != nil {
		return err
	}
	pass, err := expand("password", d.Password)
	if err != nil {
		return err
	}
	ou, err := expand("ou", d.OU)
	if err != nil {
		return err
	}

	// A username given as DOMAIN\user or user@domain carries its own domain;
	// Windows wants the account's domain separately from the domain being
	// joined, and they are usually but not always the same.
	accountDomain, account := join, user
	if i := strings.LastIndex(user, `\`); i >= 0 {
		accountDomain, account = user[:i], user[i+1:]
	} else if i := strings.LastIndex(user, "@"); i >= 0 {
		account, accountDomain = user[:i], user[i+1:]
	}

	uvars["domain_mode"] = "credentialed"
	uvars["domain_join"] = join
	uvars["domain_ou"] = ou
	uvars["domain_user"] = account
	uvars["domain_account_domain"] = accountDomain
	uvars["domain_password"] = pass
	return nil
}

// odjBlob returns the base64 provisioning data for an offline join.
//
// `djoin /provision /savefile` writes UTF-16 with a byte-order mark, while
// `/printblob` writes the same base64 as plain text, and people reasonably use
// either. Both are accepted and normalised here: a BOM reaching the answer
// file is the most common offline-join failure, and Setup reports only
// "AccountData data could not be base64 decoded" — on a machine that is no
// longer in front of you.
func odjBlob(wsDir string, d *recipe.DomainSpec, resolve func(ref string) (string, error)) (string, error) {
	var path string
	if d.BlobRef != "" {
		p, err := resolve(d.BlobRef)
		if err != nil {
			return "", fmt.Errorf("windows.domain.blob_ref %q: %w", d.BlobRef, err)
		}
		path = p
	} else if p := filepath.FromSlash(d.Blob); filepath.IsAbs(p) {
		// Workspace-relative is the documented form, but a blob often lands
		// somewhere else entirely — it is generated on a domain-joined machine
		// and copied over. Joining an absolute path to the workspace produces
		// nonsense, so take it as given.
		path = p
	} else {
		path = filepath.Join(wsDir, p)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("windows.domain: reading the offline-join blob: %w", err)
	}
	text, err := decodeODJ(raw)
	if err != nil {
		return "", fmt.Errorf("windows.domain: %s: %w", filepath.Base(path), err)
	}
	return text, nil
}

// CheckODJBlob reads an offline-join file the way a build will, so a wrong
// file is refused when it is chosen rather than several minutes into a build.
func CheckODJBlob(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(raw) > 1<<20 {
		return fmt.Errorf("%s is %d KiB, far larger than a djoin provisioning file", filepath.Base(path), len(raw)>>10)
	}
	if _, err := decodeODJ(raw); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

// decodeODJ turns a provisioning file into the base64 string the unattend
// needs, accepting UTF-16 (with or without a BOM) as well as plain text.
func decodeODJ(raw []byte) (string, error) {
	text := string(raw)
	switch {
	case len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE:
		text = decodeUTF16LE(raw[2:])
	case len(raw) >= 2 && raw[0] == 0xFE && raw[1] == 0xFF:
		return "", fmt.Errorf("the blob is UTF-16 big-endian, which djoin does not produce — is this the right file?")
	case len(raw) >= 2 && raw[1] == 0x00:
		// No BOM but NUL in the second byte: UTF-16LE all the same.
		text = decodeUTF16LE(raw)
	}
	// djoin's own output ends with a NUL that must not reach the XML.
	text = strings.TrimRight(strings.TrimSpace(text), "\x00")
	text = strings.Join(strings.Fields(text), "")
	if text == "" {
		return "", fmt.Errorf("the blob is empty")
	}
	// Checked rather than trusted: a text file that is not base64 would be
	// written into the XML and rejected during Setup, with nothing to say why.
	if _, err := base64.StdEncoding.DecodeString(text); err != nil {
		return "", fmt.Errorf("the contents are not base64 — expected the output of "+
			"`djoin /provision ... /savefile <file>` or `/printblob`: %w", err)
	}
	return text, nil
}

func decodeUTF16LE(b []byte) string {
	var sb strings.Builder
	for i := 0; i+1 < len(b); i += 2 {
		sb.WriteRune(rune(uint16(b[i]) | uint16(b[i+1])<<8))
	}
	return sb.String()
}

// checkDomainRendered fails the build when a domain join was asked for but the
// rendered unattend does not contain it — the stale-template case above.
func checkDomainRendered(rendered string, d *recipe.DomainSpec, templatePath string) error {
	if !d.Enabled() {
		return nil
	}
	marker, how := markerCredentialed, "a domain join"
	if d.Offline() {
		marker, how = markerOffline, "an offline domain join"
	}
	if strings.Contains(rendered, marker) {
		return nil
	}
	return fmt.Errorf("compose: the recipe asks for %s but %s produced no %s element — "+
		"the template predates this feature, so the machine would install into a workgroup "+
		"and look fine. Add the domain block to the template (a freshly scaffolded workspace "+
		"has it), or remove windows.domain", how, templatePath, marker)
}

// odjFileBytes writes provisioning data the way `djoin /savefile` does —
// UTF-16LE with a byte-order mark and a trailing NUL — which is what
// `djoin /requestodj /loadfile` reads, whichever form the operator supplied.
func odjFileBytes(b64 string) []byte {
	out := []byte{0xFF, 0xFE}
	for _, r := range b64 + "\x00" {
		out = append(out, byte(r), byte(r>>8))
	}
	return out
}
