package opendrive

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// uploadStage names the step a mock request belongs to, so the tests below can
// read as the protocol rather than as string matching.
func uploadStage(path string) string {
	switch {
	case strings.Contains(path, "create_file"):
		return "create"
	case strings.Contains(path, "open_file_upload"):
		return "open"
	case strings.Contains(path, "upload_file_chunk2"):
		return "chunk"
	case strings.Contains(path, "close_file_upload"):
		return "close"
	case strings.Contains(path, "/file.json"):
		return "delete"
	}
	return "other"
}

// failingUpload answers the handshake and then fails at the chunk step, calling
// onChunk first so a test can interfere.
func failingUpload(t *testing.T, onChunk func()) *mockUpstream {
	t.Helper()
	m := newMockUpstream(t)
	m.handle(func(w http.ResponseWriter, r *http.Request, _ int) {
		switch uploadStage(r.URL.Path) {
		case "create":
			_, _ = io.WriteString(w, `{"FileId":"F1","TempLocation":"tmp/1"}`)
		case "open":
			_, _ = io.WriteString(w, `{"TempLocation":"tmp/1"}`)
		case "chunk":
			if onChunk != nil {
				onChunk()
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":400,"message":"upstream fell over"}}`)
		case "delete":
			_, _ = io.WriteString(w, `{"result":true}`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	})
	return m
}

func deleteCalls(m *mockUpstream) []recordedRequest {
	var out []recordedRequest
	for _, c := range m.calls() {
		if c.Method == http.MethodDelete && uploadStage(c.Path) == "delete" {
			out = append(out, c)
		}
	}
	return out
}

func uploadClient(m *mockUpstream) *Client {
	return m.client(WithAuthenticator(&stubAuth{creds: Credentials{SessionID: "s1"}}),
		WithAccessProbe(nil))
}

func smallUpload() (io.Reader, UploadParams) {
	content := bytes.Repeat([]byte("x"), 1024)
	return bytes.NewReader(content), UploadParams{
		FolderID: "dest", Name: "orphan.txt", Size: int64(len(content)),
		ChunkSize: MinChunkSize,
	}
}

// create_file makes a real record before any content moves. A transfer that
// fails afterwards must take it back, or a cancelled bulk upload leaves one
// invisible record per file in the destination — which is how a folder ends up
// refusing writes with a message about permissions (D39).
func TestUploadReclaimsTheRecordWhenTheTransferFails(t *testing.T) {
	m := failingUpload(t, nil)
	src, p := smallUpload()

	if _, err := uploadClient(m).Uploads().Upload(context.Background(), src, p); err == nil {
		t.Fatal("want the upload to fail")
	}

	deletes := deleteCalls(m)
	if len(deletes) != 1 {
		t.Fatalf("made %d reclamation calls, want 1", len(deletes))
	}
	if !strings.HasSuffix(deletes[0].Path, "/F1") {
		t.Errorf("reclaimed %q, want the file id create_file returned", deletes[0].Path)
	}
}

// Cancellation is the case that matters most, and the one where the obvious
// implementation fails: the context the upload ran on is already dead, so the
// clean-up needs one of its own.
func TestUploadReclaimsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := failingUpload(t, cancel)
	src, p := smallUpload()

	if _, err := uploadClient(m).Uploads().Upload(ctx, src, p); err == nil {
		t.Fatal("want the upload to fail")
	}

	if got := len(deleteCalls(m)); got != 1 {
		t.Fatalf("made %d reclamation calls after cancellation, want 1", got)
	}
}

// A caller that means to resume into the record keeps it.
func TestUploadKeepsTheRecordWhenAskedTo(t *testing.T) {
	m := failingUpload(t, nil)
	src, p := smallUpload()
	p.KeepOnFailure = true

	if _, err := uploadClient(m).Uploads().Upload(context.Background(), src, p); err == nil {
		t.Fatal("want the upload to fail")
	}
	if got := len(deleteCalls(m)); got != 0 {
		t.Fatalf("made %d reclamation calls, want none: the caller asked to keep the record", got)
	}
}

// With OpenIfExists, create_file may hand back a file that already existed. It
// was never ours to delete.
func TestUploadNeverReclaimsAFileItMayNotHaveCreated(t *testing.T) {
	m := failingUpload(t, nil)
	src, p := smallUpload()
	p.OpenIfExists = true

	if _, err := uploadClient(m).Uploads().Upload(context.Background(), src, p); err == nil {
		t.Fatal("want the upload to fail")
	}
	if got := len(deleteCalls(m)); got != 0 {
		t.Fatalf("deleted a file that may have existed before this upload (%d calls)", got)
	}
}

// The record is reported before any content moves, which is what makes a
// crash-resume possible at all: the job engine has to have persisted the id
// before the process could die mid-transfer.
func TestUploadReportsTheRecordBeforeSendingAnything(t *testing.T) {
	var mu sync.Mutex
	var order []string

	m := newMockUpstream(t)
	m.handle(func(w http.ResponseWriter, r *http.Request, _ int) {
		mu.Lock()
		order = append(order, uploadStage(r.URL.Path))
		mu.Unlock()
		switch uploadStage(r.URL.Path) {
		case "create":
			_, _ = io.WriteString(w, `{"FileId":"F1","TempLocation":"tmp/1"}`)
		case "open":
			_, _ = io.WriteString(w, `{"TempLocation":"tmp/1"}`)
		case "chunk":
			_, _ = io.WriteString(w, `{"TotalWritten":1024}`)
		default:
			_, _ = io.WriteString(w, `{"FileId":"F1","Size":"1024"}`)
		}
	})

	src, p := smallUpload()
	var gotID, gotTemp string
	p.OnRecordCreated = func(fileID, tempLocation string) {
		mu.Lock()
		order = append(order, "reported")
		mu.Unlock()
		gotID, gotTemp = fileID, tempLocation
	}

	if _, err := uploadClient(m).Uploads().Upload(context.Background(), src, p); err != nil {
		t.Fatalf("upload: %v", err)
	}

	if gotID != "F1" || gotTemp != "tmp/1" {
		t.Errorf("reported (%q, %q), want the id and temp location from create_file", gotID, gotTemp)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) < 2 || order[0] != "create" || order[1] != "reported" {
		t.Fatalf("call order was %v, want the record reported straight after create_file", order)
	}
	for _, stage := range order[:2] {
		if stage == "chunk" {
			t.Fatal("content moved before the record was reported")
		}
	}
}

// Reclamation is best effort: it must never replace the error that caused it.
func TestReclamationFailureDoesNotMaskTheUploadError(t *testing.T) {
	m := newMockUpstream(t)
	m.handle(func(w http.ResponseWriter, r *http.Request, _ int) {
		switch uploadStage(r.URL.Path) {
		case "create":
			_, _ = io.WriteString(w, `{"FileId":"F1","TempLocation":"tmp/1"}`)
		case "open":
			_, _ = io.WriteString(w, `{"TempLocation":"tmp/1"}`)
		case "chunk":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":400,"message":"upstream fell over"}}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"code":500,"message":"delete failed too"}}`)
		}
	})

	src, p := smallUpload()
	_, err := uploadClient(m).Uploads().Upload(context.Background(), src, p)
	if err == nil {
		t.Fatal("want the upload to fail")
	}
	if !strings.Contains(err.Error(), "upstream fell over") {
		t.Fatalf("error = %v, want the original transfer failure", err)
	}
}

func TestReclaimNeedsAFileID(t *testing.T) {
	m := newMockUpstream(t)
	if err := uploadClient(m).Uploads().Reclaim(context.Background(), "", "", ""); err == nil {
		t.Fatal("want an error for an empty file id")
	}
}

// The clean-up is bounded, so a failing upload cannot be held open by a
// reclamation that never answers.
func TestReclamationIsBounded(t *testing.T) {
	if reclaimTimeout <= 0 || reclaimTimeout > time.Minute {
		t.Fatalf("reclaim timeout is %v; it must be short and non-zero", reclaimTimeout)
	}
}
