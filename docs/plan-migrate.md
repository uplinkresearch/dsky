# Plan: DSKY Migrate — scan an old PC, build its replacement

Dusty's spec of 2026-09-17, reconciled with what DSKY already does. The spec
said to assume nothing about the codebase and to check the repo before
duplicating anything; this file is that check, the decisions it forced, and the
order of work. **M0 is built** (see the end); M1 onward is plan.

## What already exists, and what that changes

More than half of the spec's Phase 3 is shipping code. Reading it first
changed three of the spec's own decisions.

| The spec asks for | DSKY has | What that means |
| --- | --- | --- |
| A first-boot runner: state machine, resumes across reboots, installs winget/MSI/EXE, applies settings, writes a result | `internal/agent` — compiled `dsky-agent.exe`, embedded in the media by `internal/agentbin`, steps driven by `dsky-agent.json`, `dsky-agent-state.json` so a step is never redone, `DSKY-resume` scheduled task, winget with three attempts and a per-user fallback, MSI/EXE with 3010 treated as success, `firstboot.log` + `dsky-agent.jsonl` | **Extend the agent, do not write a runner.** The migration adds steps (settings, printers, per-user, USMT) and a manifest section, not a second program. |
| A scanner that runs from removable media with no prerequisites | The agent already is exactly that: one Windows binary, no install, runs from a stick, elevation checked | **Scanner is a Go subcommand of the agent**, not PowerShell 5.1. This settles the spec's first open question: no `Add-Type` helper, no 5.1 JSON, and it is signed by the same pipeline (`build-agent.sh`, `DSKY_SIGN_CMD`) once the certificate is bought. |
| Offline domain join with `djoin`, blob in the unattend | `internal/compose/domain.go` + `internal/recipe` — offline blob, **and** a batch mode keyed by serial number, UTF-16LE handling, a build-time interlock that refuses to build if the rendered unattend lost the join, and a post-install check (`Test-ComputerSecureChannel`) | Only the provisioning half is missing: DSKY consumes a blob the operator made, it does not run `djoin /provision` itself. M4 adds that and the group add. |
| `FirstLogonCommands`, not `SetupComplete.cmd` | Already `FirstLogonCommands` → `firstboot.cmd` → the agent, with `AutoLogon` | Nothing to do. The spec's LogonCount concern is real but the agent already re-arms its own resume task and counts reboots (`MaxRestarts`). |
| USB writing, size confirmation, readback verify | `internal/flash` + `flashrun` + `fsimg`: typed-size arming, readback SHA-256, refusal to touch a fixed disk | Reuse as-is. |
| Winget IDs for common software | `internal/appcatalog`: ~95 curated packages, ID shape validation, a live check against winget-pkgs, and the operator's own MSI/EXE store | The resolver's **global** mapping table is largely this list. Name→ID matching does not exist in Go (only prefix matching in the portal's JS), so M2 is the new part. |
| A JSON Schema, validated on read and write | No schema files anywhere; the repo validates with strict Go decoding plus a `Validate()` that returns prose | **Both, with the schema generated.** Go stays the validator (it can say `mapped_drives[0]: unc "S:\shared" is not a \\server\share path`); the schema is published for other tools and generated from the types, with a test that fails when the committed file drifts. |
| An HTML report | Nothing renders HTML from data; the portal is a hand-written page fed JSON | First `html/template` in the repo. |

Two conflicts the spec could not have known about:

1. **`agentCovers` refuses domain-join recipes** (`internal/compose/agentstage.go`),
   falling back to generated PowerShell, because an offline join happens in
   `specialize` before the agent exists. Every migration in the spec's primary
   case joins a domain, so as it stands a migration would get the legacy
   scripts, which cannot read a manifest. M4 has to allow the agent alongside
   an offline join (the join is the unattend's job; the agent runs later, at
   first logon) and keep the refusal only for the credentialed path.
2. **The recipe schema has no home for a target's hostname, timezone, locale or
   accounts** — those are unattend template variables, and `recipe.Load` uses
   `KnownFields(true)`, so a new `windows.migration:` key has to be added to
   the struct rather than smuggled in. M4 decides whether a migration builds
   through a recipe (with a new section) or straight through
   `oscatalog.BuildQuick`, which already takes options rather than a recipe.

## The manifest

One file per source machine, `schema_version` major.minor, published as
`docs/migrate-manifest.schema.json` and changed under
`docs/migrate-schema-changelog.md`. Sections and rules are the spec's, with
these additions, each because the report or the review needed them:

- `source.users[]` — the profiles found, with last logon and size. The review
  has to pick whose files come across; USMT's user list is a decision, and a
  decision needs the facts beside it.
- `compat[].acknowledged` — the spec says a blocker must be acknowledged
  before approval; that has to live in the file, or reopening a manifest loses
  the answer.
- `identity.local_groups[].members` excludes well-known SIDs, as the spec says,
  and the type says so too.

Deliberate shapes worth keeping:

- `settings[].value` and `target.dsky_options` are raw JSON, kept byte for
  byte. A setting is a string, a number or a flag depending on the setting, and
  keeping the bytes means re-saving a manifest cannot change its hash.
- An empty status means "not looked at yet"; `unmapped` means "looked and found
  nothing". The resolver only touches the first, so re-running it never
  overwrites an operator's decision.
- `resolution.method: manual` is not an install. It is a line on the checklist
  the new machine prints, and it is counted apart from the installs everywhere.

**Approval is a hash, not a flag.** `content_hash` is SHA-256 over every
section but `approval`; `Approve` refuses while any blocker is unanswered or
any application unplaced; the builder recomputes and refuses a manifest that
was edited after approval, naming which of the two problems it is.

## Milestones

M0 is built. The rest keep the spec's order, with the reuse above folded in.

- **M1 — scanner.** `dsky-agent scan` (and `dsky migrate scan` on the
  operator's machine for a share-mounted target). Uninstall registry keys for
  both hives plus per-user, `Get-AppxPackage`-equivalent through the same WMI
  path the agent already uses, never `Win32_Product`. Identity from
  `Win32_ComputerSystem` + the Group Policy state key (no RSAT). Printers,
  mapped drives, SSID names only, static IPs as warnings. Settings from the
  allowlist. KFM and folder-redirection detection. Compat rules. Writes
  `manifest.json` + `report.html`. Exit codes per the spec.
- **M2 — resolver.** Site table, then the global table, then a winget index
  match (cached `source.msix` → `index.db`; thresholds 0.85 auto / 0.60
  propose). `--strict` exits non-zero with anything unplaced.
- **M3 — review.** Terminal UI first (the portal's a second pass): blockers,
  then unplaced applications one at a time, with "also save to the site table"
  defaulting to yes. Approval writes the hash. `--auto-approve` only when
  nothing is unplaced and no blocker is unanswered.
- **M4 — build.** `djoin /provision /reuse`, group add, blob into the
  unattend, payload staged (pre-downloaded winget packages, installers from
  the table, App Installer and its dependencies), the agent's manifest
  extended, `agentCovers` fixed for offline joins. Refuses an unapproved or
  edited manifest.
- **M5 — runner.** New agent steps: settings, per-app config drop, printers,
  per-user logon script, result JSON and HTML.
- **M6 — data and verify.** USMT hooks (`--usmt`, never bundled), `dsky
  migrate verify` diffing a rescan against the approved manifest, every
  missing-with-a-fuzzy-match offered as a mapping-table alias.

## Non-goals

As the spec lists them: no credentials or secrets migrate, no machine-bound
licences, no configuration beyond the curated `config_capture` list, no
settings outside the allowlist, no cloning or in-place upgrade, on-prem AD
only, and the review step is not removable. The report says all of this on
every machine, whatever the plan contains, because the person reading it did
not read this file.

Windows first, and the schema is OS-neutral on purpose (`apps[].source_kind`,
`settings[].apply` keyed by target OS) so a Linux scanner can be added without
reshaping the file.

## What M0 built

`internal/migrate` — the manifest and mapping-table types, strict load/save,
`Validate`, the approval hash, `Counts`/`InstallOrder`/`Manual`, the mapping
table with `Lookup`/`Remember`, the report renderer, and the schema generator.
`dsky migrate validate|report|schema`. Three fixtures under
`internal/migrate/testdata` — an ordinary office PC, a heavily customised one
with a blocker and an unplaced application, and a kiosk with nothing to
migrate — used by every test here and by every milestone after this one.

Divergences from the spec in M0, all small:

- `dsky manifest validate` is `dsky migrate validate`: the repo's commands are
  families (`drivers …`, `apps …`), and "manifest" already means something
  else in DSKY (a library source manifest).
- The report has no separate "summary" section; the counts are tiles at the
  top, in the portal's palette, and print legibly in black and white.
- `Lint` (advice that is not an error) is not built yet. The repo splits
  `Validate` from `Lint` in recipes and the same split is right here, but there
  is nothing to advise about until the scanner produces real manifests.
