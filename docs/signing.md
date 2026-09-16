# Code signing

What Windows shows a customer when DSKY's work reaches their machine, and what
can be done about it.

## What can be signed, and what cannot

**The agent can.** `dsky-agent.exe` is compiled here, ships inside every DSKY,
and is the program Windows raises its elevation prompt for on a customer's
machine. Sign it and that prompt names a verified publisher.

**A payload cannot.** A payload is built on the operator's own machine, out of
their recipe, their driver packs and their installers — we are not there, and
there is nothing for us to sign. Worse, a payload is the agent with an archive
appended to it, and appending to a signed program invalidates the signature,
because Authenticode covers the file as it was when it was signed. So the outer
file is unsigned whatever we do.

This is why the payload does not elevate itself. Double-clicked, it runs as
whoever clicked it, unpacks the agent alone into their temporary folder, and
asks Windows to elevate **the agent** — pointing it back at the payload with
`--from`. The prompt then names a binary we control and can sign, and the
payload it came in is only read, never elevated. Everything after that is done
by an administrator out of `C:\ProgramData\DSKY\payloads`, which is not
writable by the user whose machine it is.

## Turning signing on

`build-agent.sh` signs each agent before embedding it when `DSKY_SIGN_CMD` is
set: it runs `"$DSKY_SIGN_CMD" <file>`, which must sign the file in place. If
the file comes back the same size, the build fails rather than shipping an
agent everyone believes is signed. Set it in the release workflow from a
secret, alongside whatever the signing tool needs.

The certificate is the decision, not the plumbing:

- **Azure Trusted Signing** (~$10/month) — the key lives in Microsoft's HSM and
  signing is an API call, so it works from a CI runner with no hardware. Needs
  the LLC identity-verified, and the organisation validated (three years of
  history, or the individual option). `trusted-signing-cli` signs from Linux,
  which is where the release is built.
- **An OV certificate on a hardware token** (~$200–400/year) — since 2023 the
  private key must be on FIPS hardware, so a token has to be plugged into
  whatever machine signs. Awkward for a release that builds on GitHub.
- **Self-signed** — worth nothing on a stranger's machine, but not nothing on a
  fleet you administer: the certificate can be pushed to Trusted Publishers by
  policy, and then the prompts are clean on exactly the machines you manage.

Signing the release's own Windows binaries and `dsky-setup-amd64.exe` is the
same decision and the same certificate. Those are downloaded from GitHub, so
they carry the mark of the web and meet SmartScreen as well as UAC; a payload
carried on a stick meets only UAC.
