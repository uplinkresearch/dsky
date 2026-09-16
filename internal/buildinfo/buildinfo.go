// Package buildinfo carries version identity stamped at build time.
package buildinfo

// Version is overridden at release time via
// -ldflags "-X github.com/uplinkresearch/dsky/internal/buildinfo.Version=v0.x.y".
var Version = "v0.1.1-dev"

// UserAgent identifies DSKY in outbound HTTP requests, the way a well-behaved
// client does: product and version, and where to find out what it is.
//
// The link is not decoration. Microsoft's Download Center answered a bare
// "dsky/<version>" in about ten seconds a page and the same agent with the
// link in under one -- which turned reading the Surface driver catalog from
// twenty seconds into two minutes, close to the model picker's time limit.
func UserAgent() string { return "dsky/" + Version + " (+https://github.com/uplinkresearch/dsky)" }
