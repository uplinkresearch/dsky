# Migration manifest schema — changelog

The manifest format DSKY reads and writes when replacing a PC. The published
schema is `docs/migrate-manifest.schema.json`, generated from the Go types in
`internal/migrate` (`dsky migrate schema`), so the two cannot disagree — a test
fails when the committed file is out of date.

Versions are `major.minor`:

- **A minor bump** adds an optional field. Any build that understands major 1
  keeps reading the file.
- **A major bump** changes what an existing field means, or makes a new field
  required. Reading it needs a build that knows that major, and a migration in
  `internal/migrate/schema.go` for each earlier version.

A manifest whose major is newer than the build is refused with a sentence
saying so, rather than read with the unknown half dropped.

## 1.1 — 2026-09-18

Added `apps[].resolution.note`: why an application resolves the way it does,
when the answer needs a reason. It is carried from whichever mapping table
answered — "Edge comes with Windows", "needs the SQL Express instance first" —
and the report shows it beside anything nobody is installing. A 1.0 manifest
reads unchanged; a 1.1 manifest read by a 1.0 build would lose only the
explanation.

## 1.0 — 2026-09-17

First version. Sections: `approval`, `source`, `target`, `identity`, `apps`,
`settings`, `peripherals`, `data`, `compat`, `notes`.

Beyond the implementation spec, and why:

- `source.users[]` (profile, last logon, size). The review decides whose files
  come across; that decision needs the facts next to it.
- `compat[].acknowledged`. A blocker has to be answered before approval, so the
  answer belongs in the file — otherwise reopening a manifest loses it.
- `settings[].value` and `target.dsky_options` are raw JSON, kept exactly as
  captured, so re-saving a manifest never changes its content hash.
