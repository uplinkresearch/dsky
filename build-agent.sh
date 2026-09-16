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
LDFLAGS="-s -w -X github.com/uplinkresearch/dsky/internal/buildinfo.Version=${VERSION}"
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
