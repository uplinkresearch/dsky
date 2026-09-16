package agent

import _ "embed"

// The DSKY wordmark, as the portal and the site use it, for the top of the
// status window: the one screen a customer sees while their machine is being
// set up should look like it came from somewhere.
//
// Carried as a 7 KB PNG rather than drawn, so it is the same mark, and
// composited over the background at startup rather than blended per frame.
//
//go:embed logo.png
var logoPNG []byte
