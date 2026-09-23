package datacache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// SDKUploader sends a cached object with the SDK's upload pipeline.
//
// The flusher does not talk to OpenDrive itself, and that is the point: the
// four-call sequence, the dedupe shortcut, chunking, resumption and the record
// reclamation of D39 are all in pkg/opendrive/upload.go, tested against the live
// API, and a second implementation here would be a second set of the same bugs.
// This type is the seam that lets the gateway be tested against an upstream that
// fails on demand while shipping with the real one.
type SDKUploader struct {
	client *opendrive.Client
	// resolve finds the folder id for a path whose parent was not known when the
	// write arrived. Optional; without it an object with no folder id cannot be
	// flushed and says so.
	resolve func(ctx context.Context, remotePath string) (folderID string, err error)
}

// NewSDKUploader wraps a client.
func NewSDKUploader(c *opendrive.Client) *SDKUploader { return &SDKUploader{client: c} }

// WithFolderResolver supplies a way to find a parent folder id at flush time.
func (u *SDKUploader) WithFolderResolver(f func(context.Context, string) (string, error)) *SDKUploader {
	u.resolve = f
	return u
}

// Upload implements Uploader.
//
// The returned hash is what upstream recorded, which the flusher compares against
// the hash computed when the object was written. Returning it rather than just an
// error is deliberate: an upload that reports success is not evidence that the
// right bytes arrived, and this project keeps a list of the times upstream said
// one thing and meant another.
func (u *SDKUploader) Upload(ctx context.Context, obj *Object, content *os.File) (string, error) {
	if u.client == nil {
		return "", errors.New("datacache: no OpenDrive client to flush to")
	}
	if _, err := content.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("datacache: cannot rewind %s: %w", obj.RemotePath, err)
	}

	folderID := obj.FolderID
	if folderID == "" {
		if u.resolve == nil {
			return "", fmt.Errorf("datacache: %s has no destination folder recorded and there "+
				"is no way to look one up", obj.RemotePath)
		}
		resolved, err := u.resolve(ctx, obj.RemotePath)
		if err != nil {
			return "", err
		}
		folderID = resolved
	}

	name := obj.Name
	if name == "" {
		name = baseName(obj.RemotePath)
	}

	res, err := u.client.Uploads().Upload(ctx, content, opendrive.UploadParams{
		FolderID: folderID,
		Name:     name,
		Size:     obj.Size,
		// The hash is supplied, so the dedupe shortcut can take a file upstream
		// already has without sending a byte of it — a cache flush is exactly the
		// situation where that is most likely to pay off, because the user may
		// well have uploaded the same file from somewhere else in the meantime.
		Hash: obj.Hash,
		// A flush replaces whatever is at that path: the client wrote to it and
		// was told the write was accepted, so refusing now over a name conflict
		// would strand the object for ever.
		OpenIfExists: true,
	})
	if err != nil {
		return "", err
	}
	if res == nil || res.File == nil {
		// No file in the reply means nothing can be verified, so the flusher is
		// told as much rather than being handed an empty hash it would read as
		// "nothing to check".
		return "", fmt.Errorf("datacache: OpenDrive accepted %s but described no file, so there "+
			"is nothing to check the upload against", obj.RemotePath)
	}
	return res.File.FileHash, nil
}

// baseName is filepath.Base for a remote path, which always uses forward slashes
// whatever this machine's separator is.
func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}
