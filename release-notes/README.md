# Release notes

Adding `vX.Y.Z.md` here and landing it on `main` cuts that release:
`.github/workflows/cut-release.yml` makes an annotated tag `vX.Y.Z` on the
commit that added the file, with the file as the tag's message, and then
runs `release.yml` to build and publish it. The tag message is the release
notes, exactly as before.

Pushing an annotated `v*` tag by hand still works the same way, and a tag
that already exists is never moved.
