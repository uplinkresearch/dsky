package agent

import _ "embed"

// The DSKY wordmark, as the portal and the site use it, for the top of the
// status window: the one screen a customer sees while their machine is being
// set up should look like it came from somewhere.
//
// Carried as a 6 KB PNG rather than drawn, so it is the same mark, and
// composited over the background at startup rather than blended per frame.
//
// Quantised to 64 colours, which is a wordmark and a glow and nothing else:
// indistinguishable from the full-colour original, and smaller than the cyan
// one it replaces. Worth the line of explanation because this rides inside
// every agent, and an agent rides on every stick.
//
//go:embed logo.png
var logoPNG []byte
