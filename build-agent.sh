#!/bin/sh
# Builds the first-boot agent for Windows and embeds it, gzipped, for DSKY to
# stage onto media. Run before building DSKY itself: a DSKY built without it
# has no agent and composes media with the older generated scripts instead.
#
# Signing: the agent is the one binary in all of this that can be signed. The
# media and the payloads are built on an operator's machine, from their own
# recipes, and nothing we could sign survives that -- but the agent goes out
# with the release, and it is the program Windows raises its elevation prompt
# for. So if DSKY_SIGN_CMD is set, each agent is handed to it before being
# embedded: `"$DSKY_SIGN_CMD" <file>`, signing the file in place.
set -eu
cd "$(dirname "$0")"
VERSION="${1:-dev}"
# -H=windowsgui: the agent has no console of its own. A payload is one file
# somebody double-clicks, and Explorer gives a console-subsystem program a
# console window -- which then sits on the desktop behind the status window for
# the whole run, blank, and closable by accident. The agent attaches to the
# console it was started from when there is one, so `verify`, `scan` and a
# quiet `apply` still print for a script or a remote tool; see
# internal/agent/console_windows.go.
LDFLAGS="-s -w -H=windowsgui -X github.com/uplinkresearch/dsky/internal/buildinfo.Version=${VERSION}"
for arch in amd64 arm64; do
  out="$(mktemp)"
  CGO_ENABLED=0 GOOS=windows GOARCH="$arch" \
    go build -trimpath -ldflags "$LDFLAGS" -o "$out" ./cmd/dsky-agent
  if [ -n "${DSKY_SIGN_CMD:-}" ]; then
    before="$(wc -c < "$out")"
    "$DSKY_SIGN_CMD" "$out"
    after="$(wc -c < "$out")"
    # A signature is bytes added to the file. If the file came back the same
    # size, nothing signed it, and shipping an agent that everybody believes
    # is signed is worse than shipping one nobody believes is.
    if [ "$before" = "$after" ]; then
      echo "build-agent: DSKY_SIGN_CMD left $arch unchanged; it did not sign it" >&2
      exit 1
    fi
    printf 'agent %s: signed (%s -> %s bytes)\n' "$arch" "$before" "$after"
  fi
  # -n: no timestamp or name in the header, so identical input gives an
  # identical file and DSKY's reproducible builds stay reproducible.
  gzip -9 -n -c "$out" > "internal/agentbin/bin/dsky-agent-${arch}.exe.gz"
  rm -f "$out"
  printf 'agent %s: %s bytes\n' "$arch" "$(wc -c < "internal/agentbin/bin/dsky-agent-${arch}.exe.gz")"
done

# What the agent was built from, so a stale embedded agent is caught before it
# reaches media rather than on a machine. See internal/agentbin/staleness_test.go.
# LC_ALL=C, because this has to agree with the Go side in staleness_test.go and
# Go sorts by byte. `sort` sorts by the locale's collation instead, and on
# macOS that is a different order from glibc's -- which is why this hash and
# the test's disagreed there and nowhere else, turning main red on one platform
# with an error message about a stale agent that was not stale at all.
#
# The same reason the file list is sorted at all: two people's checkouts must
# hash to the same thing, so the order cannot come from the filesystem or from
# whatever locale somebody happens to have set.
find internal/agent cmd/dsky-agent -name '*.go' ! -name '*_test.go' | LC_ALL=C sort | xargs cat |
  sha256sum | cut -d' ' -f1 > internal/agentbin/bin/sources.sha256
printf 'agent sources: %s\n' "$(cut -c1-12 internal/agentbin/bin/sources.sha256)"
