//go:build integration

package opendrive

import (
	"fmt"
	"testing"
	"time"
)

// D52, kept true on the wire: until close_file_upload, file/info reports an
// upload record with Size 0 and no FileHash — even with every byte sent — and
// after it, the declared size and the content's MD5. The job engine relies on
// this to tell an empty record it may delete from a finished file it must not.
func TestSandboxAnUnclosedRecordHasNoSizeOrHash(t *testing.T) {
	c, ctx := probeClient(t)
	folder := probeScratch(t, ctx, c)
	up := c.Uploads()
	content := probePayload(256<<10, fmt.Sprint(time.Now().UnixNano()))
	hash := probeMD5(content)
	p := UploadParams{Size: int64(len(content)), Hash: hash}

	created, err := up.createFile(ctx, folder, UploadParams{Name: "d52.bin", Size: p.Size, Hash: hash})
	if err != nil {
		t.Skipf("create_file refused: %v", err)
	}
	id := created.FileID.String()
	t.Cleanup(func() { _ = up.Reclaim(ctx, id, "", "") })

	opened, err := up.openFile(ctx, id, p)
	if err != nil {
		t.Skipf("open_file_upload refused: %v", err)
	}
	if _, err := up.sendOneChunk(ctx, id, opened.TempLocation, 0, content); err != nil {
		t.Fatalf("chunk: %v", err)
	}
	before, err := c.Files().Info(ctx, id)
	if err != nil {
		t.Fatalf("info before close: %v", err)
	}
	if before.Size.Int64() != 0 || before.FileHash != "" {
		t.Errorf("an unclosed record reports Size %d, FileHash %q; D52 says 0 and empty, and "+
			"the job engine's reclaim check depends on it", before.Size.Int64(), before.FileHash)
	}

	if _, err := up.closeFile(ctx, id, opened.TempLocation, p, false); err != nil {
		t.Fatalf("close: %v", err)
	}
	after, err := c.Files().Info(ctx, id)
	if err != nil {
		t.Fatalf("info after close: %v", err)
	}
	if after.Size.Int64() != p.Size || after.FileHash != hash {
		t.Errorf("a closed record reports Size %d, FileHash %q; want %d, %s",
			after.Size.Int64(), after.FileHash, p.Size, hash)
	}
}
