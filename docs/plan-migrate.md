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

- **M1 — scanner. Built** (see "What M1 built" below).
- **M2 — resolver. Built** (see "What M2 built" below).
- **M3 — review. Built** (see "What M3 built" below).
- **M4 — build. Done, including the defect it walked into**: a domain-joined
  machine could not finish OOBE unattended at all, which turned out to be one
  defect rather than two and is fixed by joining after OOBE instead of during
  it (see below). Still to do: pre-downloaded winget packages and the App
  Installer bundle for machines with no internet at first boot.
- **M5 — runner. Done**, apart from the per-app config drop, which is
  **blocked, not deferred**: it restores files nothing collects yet. Settings,
  printers, mapped drives, the per-user logon script and the result report are
  built; see "What M5 built" below.
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

## What M5 built, and the item it cannot build yet

The runner carries out the parts of a plan that are not programs, and the one
decision running through all of it is **who a thing belongs to**.

The agent runs at first boot as a local administrator nobody will ever sign in
as. Half of what a person notices about their PC lives in HKCU — file
extensions, hidden files, the taskbar search box — and a shared printer queue
and a mapped drive are theirs too. Applying any of that as the agent would work
perfectly, in an account that does not matter, and leave the person who sits
down to a machine that looks nothing like the one they had. It would also look
like it had worked.

So the plan splits in two. What belongs to the machine — power plan, time zone,
locale, region, a printer with its own address — the agent applies. What
belongs to a person is staged: a fixed script under the machine's `Run` key, a
typed data file beside it (`reg|`, `printer|`, `drive|`), and a marker in each
person's own profile so it runs once for them and is a no-op afterwards. It
never removes itself, because a machine has more than one user and the second
person to sit down deserves the same settings as the first. Explorer is
restarted at the end, because it reads most of those values once, at sign-in.

The script is byte-identical on every machine DSKY makes; everything that
differs is a line in a text file. That is the rule this package exists for:
text assembled per build cannot be tested per build.

The build says which of these arrive later, because an operator checking a
machine at the bench will not see them and would otherwise reasonably conclude
they had not worked. Direct printers get a warning of their own: they need a
driver a new Windows 11 probably does not have, and when one cannot be added
the first-boot log names the printer, names the missing driver, and says to
install it and add the queue by hand — which is exactly what happens next.

Settings and printers reach the build through the build request rather than the
recipe. They belong to one machine's approved plan, not to a recipe somebody
reuses, and the recipe schema has no business learning what a migration is.

**The per-app config drop cannot be built yet, and that is worth being exact
about.** `config_capture` records *where* a known application keeps its
settings on the old machine. Nothing collects those files: the scanner only
reads, by design ("no install on the source machine"), and the copying is the
file-migration work in M6. So a drop step written now would be code against
data nothing produces — it could not be tested, and the first real test would
be somebody's machine. It waits for M6, where the files start existing.

The result report is done, in two halves. The agent writes
`dsky-migrate-result.json` on the machine — after every step, not at the end,
because a migration guarantees a restart and a record written only at the end
is never written on exactly the machines this exists for. `dsky migrate result`
renders it as a page, with the plan alongside so a program is called what
people call it rather than which file was run.

Three states, and the third is the one that matters: `done`, `failed`, and
`at-sign-in` for everything belonging to a person and waiting for them to turn
up. Calling those done would be a lie; calling them failed would send a
technician looking for a fault that is not there. The page leads with what
needs a hand, then explains the waiting ones as not being faults, then lists
what is finished — because the question somebody reads it with is "what do I
have to do?". It exits non-zero when anything needs a hand, so a bench of
machines can be swept without opening every page.

M5 is therefore done except for the config drop, which is blocked above.

## What M4 built, and what it found

`dsky migrate build` refuses a plan nobody approved, one edited after approval,
one with an application nobody placed, or a domain machine with no OU; asks for
(or runs) `djoin /provision` for that one machine; copies the plan's installers
into the library the way an operator's own .msi goes in; and hands the rest to
the pipeline that has been building Windows sticks all along. `agentCovers`
now allows the first-boot agent alongside a single offline join file, which is
the conflict this plan flagged on day one: Setup performs that join in
specialize, long before anything runs at first logon, and refusing it meant
every migration fell back to the generated scripts, which cannot read a
manifest.

A credentialed join and a by-serial batch still keep the generated scripts. The
by-serial refusal is not caution: the generated first boot is what reports a
join that failed, and handing that boot to the agent would make a failed join
silent. The agent grows its own domain check with the runner (M5), and that is
when by-serial can be revisited.

Proven against the lab: a computer account provisioned on the Server 2025 DC,
the join file carried into the media, and the media checked for what it should
contain — the blob in `AccountData`, the old machine's time zone, the local
administrator's name, no `ComputerName` (the blob names the machine), the
staged installer, and `firstboot.cmd` reduced to the agent handoff. Booted on a
fresh VM, **the offline domain join worked**: `NetSetup.LOG` says
"Provisioning package installation completed successfully", and the machine's
logon screen offers "Sign in to: LAB".

**And then it stops, for two reasons that are Windows' and not this feature's.**
Both affect any DSKY media that joins a domain, not just a migration:

1. **OOBE refuses to set automatic sign-in for a local account on a
   domain-joined machine.** Its own log says so: "Not setting autologon for new
   local user. e.g. upgrade, domain-joined, or system-managed user". The
   machine reaches a logon screen and stops, with none of the first boot run —
   no agent, no programs, nothing.
2. **Windows 11 OOBE puts up "Why did my PC restart?" and waits for a click.**
   On the first machine one click let it continue; on the second it stayed put
   even after a click that visibly landed. Unattended media cannot depend on
   somebody clicking.

The first attempt at (1) wrote the same intent (`AutoAdminLogon`,
`DefaultUserName`, `DefaultDomainName`, `AutoLogonCount`) straight to the
registry in the specialize pass, on the theory that Winlogon reads those values
whatever OOBE thinks. **Reading the machine's disk afterwards disproved it**,
and the way it did is worth keeping:

- The commands are on the media exactly as intended, and they ran: the
  specialize log shows all five RunSynchronous commands returning `0x0`.
- Twenty-two minutes later, OOBE logs its refusal — and then, on the next
  line, **"Transferring unattend.xml autologon values"**. That is OOBE writing
  the same registry values itself, after specialize. Anything specialize put
  there is behind OOBE in the queue, so the pass is simply too early.
- The account was never created (`C:\Users` holds only `defaultuser0`, OOBE's
  own temporary account) and no `firstboot.log` exists, so nothing of the first
  boot ran — while the agent, its manifest and the staged installer are all
  sitting in `C:\Windows\Setup\Scripts`, which confirms the media handoff
  itself works.
- OOBE **did not crash**: every task it started ended "successfully", the
  zero-day-patch scan completed ("Found 0 updates"), and the log simply stops
  where the VM was killed. So "Why did my PC restart?" is a page OOBE chose to
  show and wait on, not the aftermath of a failure — which rules out the
  theory that a blank administrator password was crashing it.

So the sign-in has to be re-established *after* OOBE is done with those values;
`SetupComplete.cmd` is the hook, and it is deliberately not wired up yet,
because nothing after OOBE runs at all until (2) is solved. The specialize
commands are kept: they are correct, harmless, and the next attempt needs them
there to see what OOBE does to them.

**They then turned out to be one defect.** Reading the same log end to end,
with the line numbers of all three OOBE runs, gives the whole loop:

```
02:05:32 [Shell Unattend] UserAccounts: created account 'uplink'
02:05:32 [Shell Unattend] AutoLogon Username is 'uplink'
02:05:33 [msoobe.exe]     Not setting autologon for new local user ... domain-joined
02:06:36 [msoobe.exe]     PostTaskOperations with accountNeeded = 1
02:06:36 [msoobe.exe]     Post-setup for the default account ... m_defaultAccountName = defaultuser0
02:06:37                  image state [UNDEPLOYABLE] --> [COMPLETE]      <- OOBE finished
02:09:21 [taskhostw.exe]  OOBE Monitor event received: 101
02:11:21 [taskhostw.exe]  UserOOBEController::Exit() started [2]
02:11:21                  image state [COMPLETE] --> [SPECIALIZE_RESEAL_TO_OOBE]
02:11:44                  WinDeploy relaunches -> OOBE run 2 ... and run 3
```

The answer file is honoured: the account is created, added to Administrators,
given its password, and named as the automatic sign-in. Then msoobe overrides
that because the machine is domain-joined, hands the sign-in to `defaultuser0`
to run *user* OOBE instead, and user OOBE has no flow it can complete without
a person. `UserOOBEController::Exit() [2]` resets the image state and the
machine reboots back into OOBE. Three times, and it would have gone on.

So "Why did my PC restart?" is not a page to get past — it is the machine
explaining a reset it will keep doing. Clicking it was never going to help.
The single cause is **being domain-joined while OOBE runs**.

(`C:\Users\uplink` being absent was a red herring and nearly sent this the
wrong way: a profile directory is only made at first sign-in. The account was
there all along, in the SAM. Check the SAM hive, not `C:\Users`.)

**The fix: join after OOBE, not during it.** The answer file gets no join
element at all. Setup installs Windows into a workgroup, OOBE takes the path
DSKY has always shipped and signs itself in, and the agent applies the blob as
its first step with `djoin /requestodj /loadfile <file> /windowspath
%SystemRoot% /localos`, then asks for the restart it needs. The agent's own
restart machinery carries that out between steps and resumes afterwards, so
the rest of the build happens on a machine that is by then a domain member.
First rather than last, because a machine that has to restart should do it
before an hour of installing programs.

The generated first-boot script could not have done this: a migration is the
case that needs the agent and its manifest. So `agentCovers` stops needing an
exception for offline joins at all, and M5's "move the domain-join check into
the agent" arrives here instead, for a better reason.

Not `SetupComplete.cmd`, which is where the previous attempt was heading:
`internal/recipe/lint.go` already refuses it, because Windows skips it under a
firmware OEM key — exactly the hardware a practice's replacement PC has. It
would have passed in this lab and failed at a customer.

**Proven, on media built by this code and booted in the lab:**

```
RESULT Name=NEWDESK01  Domain=lab.dsky.local  Joined=True  User=newdesk01\user
```

with the finish screen reading OK domain, OK Drivers, OK Removing preinstalled
extras, "Set up in 8 minutes". No restart loop, no page to click. The second
sign-in — the one after the join, as a local account on what is by then a
domain member — is Winlogon's rather than OOBE's, which is the premise the
whole fix rests on, and it holds.

That `RESULT` line settled one more thing. The machine was built with **no
name of its own** (the answer file's default is `*`, meaning random) and came
back named `NEWDESK01`: **djoin renames the machine from the blob even when
applied to a running OS.** The build had been changed to send the target
hostname as belt and braces, on the theory that something has to rename a
machine that is already up; that is the configuration nobody tested, so it
came back out.

**A by-serial batch is not immune, and an earlier note here said it was.**
`internal/recipe/domainserial.go` runs `djoin` in the specialize pass too, so
those machines are domain members before OOBE exactly as this one was, and are
exposed to the same loop whenever they also sign themselves in. Nothing here
changes that path. It is worth a lab run of its own before anyone images a
batch with it.

A separate finding, independent of both: this build was making a **passwordless
local administrator that signs itself in automatically on a domain member** —
anybody who walked past the new PC before somebody collected it would get an
administrator's desktop on a machine the domain already trusts. Fine on a
standalone box in front of you, which is why the quick install has always
allowed it; not fine here. `dsky migrate build` now refuses a domain build
without a password and takes it on stdin or from `DSKY_ADMIN_PASSWORD`, never
as a flag value that would land in shell history. It is not written to the
manifest (a document people mail around) and not to the recipe the build
leaves in the library: the recipe says `${var:admin_password}` and the value
travels in memory. That also gives the automatic sign-in a `DefaultPassword`
to use, which it had no way to work without — but it is not a fix for (2), and
the log above is why.

## What M3 built

`dsky migrate review` is the step the spec refuses to remove, so the work was
making a person's part short rather than clever. It asks in the order that can
stop a build: blockers, then each application nobody could place, then the two
or three facts a scan cannot know (where a USMT store lives, whether the new
machine joins a different OU), then a note for the report, then approval.

Every answer is offered back to the site's table, defaulting to yes, because
the expensive part of a migration is working out where a practice's own
software comes from and nobody should pay for it twice. Proven on the lab
machine: reviewing it once taught the table its practice software, and a
second machine at that site resolved with no questions at all.

Three things the running of it settled:

- **Enter never decides.** On an application's question it means "decide
  later", so somebody holding Enter down a long list cannot agree to installs
  they never read. Accepting the resolver's suggestion is one key (`y`), which
  is the common case.
- **A wrong key says so.** Silently re-asking the same question reads like a
  stuck program; it now names what it did not understand.
- **"Accept the risk" has to mean something the plan says out loud.** Accepting
  the missing-USMT blocker used to leave a plan still claiming it would copy
  somebody's files with nothing to copy them with — noticed, if at all, as an
  empty Documents folder on the new machine. It now switches the plan to "not
  migrated" and writes that into the notes the report prints.

`--auto-approve` is the repeat-machine path: no questions, and it refuses
exactly what the review refuses. On the lab's second machine it refused,
correctly, because the USMT blocker was unanswered.

## What M2 built

`dsky migrate resolve` fills in where each application comes from, from three
places in order of how much each knows about this customer: the site's own
table, the table DSKY ships, and the names themselves.

**The bundled table is the app picker's list.** `internal/appcatalog` already
holds ninety-odd packages whose winget ids are re-checked against winget-pkgs
every week; building the global table from it means the two cannot drift and a
package added for the picker helps a migration the same day. What this feature
adds of its own is only what a picker does not need: the runtimes nobody
chooses (Visual C++, .NET desktop, WebView2 — their ids checked live before
committing, and added to the weekly catalog-health run), the PC makers' own
utilities and Windows' own apps as things to leave behind, and the software
DSKY knows winget does not have, which comes out as "a person installs this"
with the note saying where from.

**The name matcher is last and says how sure it is.** "Notepad++ (64-bit x64)"
in a registry is the "Notepad++" DSKY knows; the score is how much of the
shorter name the two share, with the longer side's version, architecture and
edition words thrown away first. At or above 0.85 it is the answer; between
0.60 and 0.85 it is written as a suggestion with the status left unmapped, so
the review still asks but the answer is already typed in; below that nothing
is said at all.

**Running it again is safe, and is the point.** An application a person
settled is never reopened. One the tool itself failed to place is tried again
— because the reason to re-run is usually that somebody has just added the
entry that places it. "Nobody has looked" and "looked and found nothing" are
different states in the file for exactly this reason.

Two things the real lab machine changed:

- **One package installs once.** A machine with 7-Zip registered in both hives
  has two applications in the manifest, both true and both in the report, and
  they resolved to one winget id. The install order deduplicates by what would
  actually be run.
- **"Left behind, decided by mapping_table" is a fact with no reason**, which
  is the sort of line a customer queries. `resolution.note` (schema 1.1)
  carries the answering table's note — "Edge comes with Windows", "needs the
  SQL Express instance first" — into the manifest and the report.

The lab client, with one site-table entry for its practice software, resolves
completely: 3 installs (7-Zip once, Notepad++, the practice software from the
site's share), 2 left behind with reasons, nothing unplaced.

**The winget index is deliberately not built yet.** The spec's third source is
the full package index, which Microsoft publishes only as a SQLite file inside
`source.msix`; reading it needs either cgo (this repo builds CGO_ENABLED=0) or
a pure-Go SQLite, which is a large dependency for a lean tree. The resolver
takes a list of `Resolver`s, so adding it later is one more implementation and
one line at the call site. Until then the curated list plus name matching
covers ordinary office software, and everything else is the review's work —
which is where a person would end up with a fuzzy index match anyway.

## What M1 built

`dsky-agent scan` on the machine being replaced, `dsky migrate scan` for a
technician sitting at one, both through `internal/migrate`'s one collector so
the path used on a customer's desk is not the less-tested one. The readings
are one PowerShell 5.1 script printing one JSON document; the Go side parses
it, decides nothing about it that a person should decide, and writes
`manifest.json`, `report.html` and `scan.log`.

It was run against the lab (see [[reference-dsky-migrate-lab]]): a Windows 10
22H2 client joined to lab.dsky.local, carrying 7-Zip in both architectures, a
32-bit line-of-business application with no installer anywhere, a printer on
an IP port, a static address, settings somebody changed, and a domain profile
beside a local one. **Six defects came out of that run that no unit test
would have found**, and they are the reason M1 took a second pass:

1. **One locked hive lost every application.** `reg load` of an unloaded
   profile failed ("the profile is in use"), `$ErrorActionPreference = Stop`
   turned that into a terminating error, and the manifest came back with zero
   applications and a one-line note. Each hive is now its own attempt, and
   what it could not read is a note naming whose software is missing.
2. **The scan took over twenty minutes.** `Get-WindowsDriver -Online -All`
   (DISM) was the whole of it. `pnputil /enum-drivers` answers in 0.1s and
   carries the signer name, which is what the rule actually needs.
3. **Eleven of the twelve warnings were Windows' own inbox drivers**, which
   DISM reports as unsigned. Only a third-party driver with no signer is worth
   a person's attention, and that is what the rule says now.
4. **`Win32_NTDomain` cost 15.6 of the remaining 24 seconds** — it goes to a
   domain controller for the NetBIOS name. That name is the first label of the
   DNS name, upper case, and the review is where somebody can correct the
   rename case. The scan now finishes in **7.5 seconds**.
5. **Microsoft Edge was reported as 32-bit** because it registers only in the
   32-bit hive and installs under Program Files (x86). The hive is not
   evidence: the PE header of the application's own largest executable is,
   and it is two bytes after a short seek.
6. **Noise a customer would have read as findings**: Windows' own print queues
   (Print to PDF, XPS, Fax, OneNote) outnumbered the two real printers three
   to one; every local group was Windows' defaults (Guest, DefaultAccount, the
   local Administrator, and the two memberships a domain join makes by
   itself); Windows servicing entries sat in the application list; and the
   two 7-Zips were reported as one 32-bit application with no 64-bit version
   while its 64-bit twin was two rows above it (cutting a name at its first
   digit leaves "7-Zip 24.08" whole, because the 7 is the first character).

What the lab client's plan looks like now: 6 applications, 10 settings
captured (9 appliable), 2 printers, the OU `OU=Front Desk,OU=Workstations,
DC=lab,DC=dsky,DC=local` for djoin, `LAB\reception` as the one hand-added
group member, USMT proposed for the domain profile and not for the local
account, and one blocker — USMT itself is not on the stick.

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
