package compose

import (
	"context"
	"os"
	"time"

	"github.com/uplinkresearch/dsky/internal/buildinfo"
	"github.com/uplinkresearch/dsky/internal/stream"
)

// buildRaw handles linux-iso and raw-img recipes: the artifact IS the blob
// (no copy); compression is detected by magic bytes and recorded so the
// flash engine can stream-decompress.
func buildRaw(ctx context.Context, req Request) (*Artifact, error) {
	r := req.Recipe
	src, err := req.Workspace.Source(r.OS.Source)
	if err != nil {
		return nil, err
	}
	entry, err := req.Library.Resolve(src.ID)
	if err != nil {
		return nil, err
	}
	blob := req.Library.BlobPath(entry.SHA256)
	comp, err := stream.Detect(blob)
	if err != nil {
		return nil, err
	}
	if r.Linux != nil && r.Linux.Autoinstall != nil {
		return buildLinuxAutoinstall(ctx, req, entry, blob, comp)
	}
	if r.Linux != nil && r.Linux.Kickstart != nil {
		return buildLinuxKickstart(ctx, req, entry, blob, comp)
	}
	st, err := os.Stat(blob)
	if err != nil {
		return nil, err
	}
	key, err := inputsKey(req, entry.SHA256)
	if err != nil {
		return nil, err
	}
	// A compressed image's file size is not the size it writes to a stick,
	// so only an uncompressed one can raise the minimum; for the rest the
	// recipe's figure stands, as it always has.
	var written int64
	if comp == "" || comp == "none" {
		written = st.Size()
	}
	a := &Artifact{
		RecipeID: r.ID, Kind: "raw", Path: blob, Size: st.Size(),
		SHA256: entry.SHA256, Compress: comp, InputsKey: key,
		Verify: r.Flash.Verify, MinStick: minStickBytes(r, written),
		CreatedAt: nowUTC(), Tool: toolVersion(),
	}
	// Raw artifacts get no sidecar next to the blob (blobs are content-
	// addressed and shared); the caller passes the Artifact straight to
	// flash.
	return a, nil
}

func nowUTC() time.Time   { return time.Now().UTC() }
func toolVersion() string { return buildinfo.Version }
