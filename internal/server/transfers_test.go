package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/echotreez/opendrive-bridge/internal/jobs"
	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// withTransfers adds the upload/download machinery to the fake upstream and
// gives the server a job engine, so the transfer endpoints can be exercised
// without touching the network.
func (u *fakeUpstream) withTransfers(content []byte) {
	base := u.srv.Config.Handler
	u.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.Contains(p, "create_file"):
			_, _ = io.WriteString(w, `{"FileId":"UP1","TempLocation":"tmp/1"}`)
		case strings.Contains(p, "open_file_upload"):
			_, _ = io.WriteString(w, `{"TempLocation":"tmp/1"}`)
		case strings.Contains(p, "upload_file_chunk2"):
			n := r.URL.Query().Get("chunk_size")
			_, _ = fmt.Fprintf(w, `{"TotalWritten":%s}`, n)
		case strings.Contains(p, "close_file_upload"):
			_, _ = io.WriteString(w, `{"FileId":"UP1","Name":"x","Size":"10"}`)
		case strings.Contains(p, "download/file.json"):
			if r.URL.Query().Get("test") == "1" {
				_, _ = io.WriteString(w, `{"result":true,"dl_stream_status":true}`)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(content)
		case strings.Contains(p, "download/all.json"):
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write([]byte("PK\x03\x04 pretend archive"))
		case strings.Contains(p, "expiringlink"):
			_, _ = io.WriteString(w,
				`{"Link":"https://od.lk/f/ABC","ExpiringDate":"2026-12-31","Counter":"0","CounterMax":"5"}`)
		default:
			base.ServeHTTP(w, r)
		}
	})
}

func (u *fakeUpstream) serverWithEngine(t *testing.T) (*Server, *jobs.Engine) {
	t.Helper()
	srv := u.server(t)
	engine, err := jobs.New(srv.client, jobs.WithWorkers(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Stop)
	engine.Start(context.Background())
	srv.engine = engine
	return srv, engine
}

func waitJob(t *testing.T, srv *Server, id string) *jobs.Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		j, err := srv.engine.Get(id)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		if j.State.Terminal() {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s stayed %s", id, j.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestUploadQueuesAJobAndReportsIt(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv, _ := u.serverWithEngine(t)

	local := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(local, bytes.Repeat([]byte("x"), 512), 0o600); err != nil {
		t.Fatal(err)
	}

	rec, body := do(t, srv, http.MethodPost, "/v1/upload",
		fmt.Sprintf(`{"local_path":%q,"remote_path":"/Docs/new.bin"}`, local))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	// The response is the engine's own job shape (§4.3), not a copy of it.
	for _, field := range []string{"id", "kind", "state", "bytes_done", "bytes_total", "speed"} {
		if _, ok := body[field]; !ok {
			t.Errorf("%q missing from the queued job", field)
		}
	}
	if body["kind"] != "upload" {
		t.Errorf("kind = %v", body["kind"])
	}
	waitJob(t, srv, body["id"].(string))
}

// Uploading over something that is already there needs saying so.
func TestUploadRefusesToOverwriteSilently(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv, _ := u.serverWithEngine(t)

	local := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(local, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec, body := do(t, srv, http.MethodPost, "/v1/upload",
		fmt.Sprintf(`{"local_path":%q,"remote_path":"/Docs/report.pdf"}`, local))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	msg := body["error"].(map[string]any)["message"].(string)
	assertUserReadable(t, msg)
	if !strings.Contains(msg, "overwrite") {
		t.Errorf("the message does not say how to proceed: %q", msg)
	}

	// With overwrite it goes through.
	rec2, _ := do(t, srv, http.MethodPost, "/v1/upload",
		fmt.Sprintf(`{"local_path":%q,"remote_path":"/Docs/report.pdf","overwrite":true}`, local))
	if rec2.Code != http.StatusAccepted {
		t.Errorf("status = %d with overwrite: %s", rec2.Code, rec2.Body)
	}
}

func TestUploadValidatesItsInputs(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv, _ := u.serverWithEngine(t)
	dir := t.TempDir()

	// A real local file, made here rather than borrowed from the operating
	// system. It was /etc/hosts once, until a CI runner pointed out that Windows
	// has no such file; borrowing from the host was the mistake either way, since
	// a test that depends on the machine's contents fails for reasons that have
	// nothing to do with the code.
	real := filepath.Join(dir, "real.bin")
	if err := os.WriteFile(real, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "definitely-not-here.bin")

	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"no local path", `{"remote_path":"/Docs/x.bin"}`, http.StatusBadRequest},
		{"missing local file", fmt.Sprintf(`{"local_path":%q,"remote_path":"/Docs/x.bin"}`, missing), http.StatusBadRequest},
		{"a folder as the source", fmt.Sprintf(`{"local_path":%q,"remote_path":"/Docs/x.bin"}`, dir), http.StatusBadRequest},
		{"missing remote folder", fmt.Sprintf(`{"local_path":%q,"remote_path":"/Nope/x.bin"}`, real), http.StatusNotFound},
	} {
		rec, body := do(t, srv, http.MethodPost, "/v1/upload", tc.body)
		if rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body)
			continue
		}
		assertUserReadable(t, body["error"].(map[string]any)["message"].(string))
	}
}

func TestUploadStreamTakesTheBody(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv := u.server(t)

	content := bytes.Repeat([]byte("y"), 300)
	r := httptest.NewRequest(http.MethodPut, "/v1/upload/stream?path=/Docs/streamed.bin",
		bytes.NewReader(content))
	r.ContentLength = int64(len(content))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"md5"`) {
		t.Errorf("the response does not report the digest: %s", rec.Body)
	}
}

// Upstream needs the size before the first byte, so a chunked body cannot work
// and the caller has to be told why rather than meeting a confusing failure.
func TestUploadStreamNeedsALength(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv := u.server(t)

	r := httptest.NewRequest(http.MethodPut, "/v1/upload/stream?path=/Docs/streamed.bin",
		strings.NewReader("data"))
	r.ContentLength = -1
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Content-Length") {
		t.Errorf("the message does not name what is missing: %s", rec.Body)
	}
}

func TestDownloadQueuesAJob(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers([]byte("downloadable"))
	srv, _ := u.serverWithEngine(t)

	local := filepath.Join(t.TempDir(), "out.bin")
	rec, body := do(t, srv, http.MethodPost, "/v1/download",
		fmt.Sprintf(`{"remote_path":"/Docs/report.pdf","local_path":%q}`, local))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	job := waitJob(t, srv, body["id"].(string))
	if job.State != jobs.StateSucceeded {
		t.Fatalf("job %s: %+v", job.State, job.Error)
	}
	got, err := os.ReadFile(local) //nolint:gosec // test temp file
	if err != nil || string(got) != "downloadable" {
		t.Errorf("downloaded %q (%v)", got, err)
	}
}

func TestDownloadRefusesAFolder(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv, _ := u.serverWithEngine(t)

	rec, body := do(t, srv, http.MethodPost, "/v1/download",
		`{"remote_path":"/Docs/2026","local_path":"/tmp/x"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	msg := body["error"].(map[string]any)["message"].(string)
	assertUserReadable(t, msg)
	if !strings.Contains(msg, "archive") {
		t.Errorf("the message does not point at the alternative: %q", msg)
	}
}

func TestDownloadStreamSendsTheContent(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers([]byte("streamed content"))
	srv := u.server(t)

	r := httptest.NewRequest(http.MethodGet, "/v1/download/stream?path=/Docs/report.pdf", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if rec.Body.String() != "streamed content" {
		t.Errorf("body = %q", rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Content-Disposition"), "report.pdf") {
		t.Errorf("no filename offered: %q", rec.Header().Get("Content-Disposition"))
	}
}

// A failure before any byte is written must still answer in the envelope, not
// as a truncated file the caller would save and only later find broken.
func TestDownloadStreamFailsInTheEnvelope(t *testing.T) {
	u := newFakeUpstream(t)
	base := u.srv.Config.Handler
	u.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "download/file.json") {
			// The realistic failure: upstream refuses in JSON before sending
			// any content.
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":404,"message":"File is deleted"}}`)
			return
		}
		base.ServeHTTP(w, r)
	})
	srv := u.server(t)

	r := httptest.NewRequest(http.MethodGet, "/v1/download/stream?path=/Docs/report.pdf", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)

	if rec.Code == http.StatusOK {
		t.Fatalf("a failed download answered 200: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("the failure is not in the envelope: %s", rec.Body)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want the classifier's not_found", rec.Code)
	}
}

func TestArchiveDownloadsAFolder(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv := u.server(t)

	r := httptest.NewRequest(http.MethodGet, "/v1/download/archive?path=/Docs", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if !strings.HasPrefix(rec.Body.String(), "PK") {
		t.Errorf("body is not an archive: %q", rec.Body.String()[:8])
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("content type = %q", ct)
	}
}

func TestArchiveRefusesAFile(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv := u.server(t)

	rec, body := do(t, srv, http.MethodGet, "/v1/download/archive?path=/Docs/report.pdf", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	assertUserReadable(t, body["error"].(map[string]any)["message"].(string))
}

func TestJobsListAndCancel(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv, _ := u.serverWithEngine(t)

	local := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(local, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, queued := do(t, srv, http.MethodPost, "/v1/upload",
		fmt.Sprintf(`{"local_path":%q,"remote_path":"/Docs/one.bin"}`, local))
	id := queued["id"].(string)
	waitJob(t, srv, id)

	_, list := do(t, srv, http.MethodGet, "/v1/jobs", "")
	if len(list["jobs"].([]any)) != 1 {
		t.Errorf("jobs = %v", list["jobs"])
	}

	rec, one := do(t, srv, http.MethodGet, "/v1/jobs/"+id, "")
	if rec.Code != http.StatusOK || one["id"] != id {
		t.Errorf("status = %d body = %v", rec.Code, one)
	}

	// Cancelling a job that has already finished is not an error.
	recCancel, _ := do(t, srv, http.MethodDelete, "/v1/jobs/"+id, "")
	if recCancel.Code != http.StatusOK {
		t.Errorf("cancel status = %d", recCancel.Code)
	}
}

func TestUnknownJobIsReportedPlainly(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv, _ := u.serverWithEngine(t)

	rec, body := do(t, srv, http.MethodGet, "/v1/jobs/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	assertUserReadable(t, body["error"].(map[string]any)["message"].(string))
}

// ---------------------------------------------------------------- sharing

func TestShareCreateReturnsALink(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv := u.server(t)

	rec, body := do(t, srv, http.MethodPost, "/v1/share/link",
		`{"path":"/Docs/report.pdf","expires_at":"2026-12-31","max_uses":5}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if body["url"] != "https://od.lk/f/ABC" {
		t.Errorf("url = %v", body["url"])
	}
	if body["expires_at"] != "2026-12-31" {
		t.Errorf("expires_at = %v", body["expires_at"])
	}
}

// Upstream takes a date rather than a timestamp, so the Bridge does too — and
// says so plainly instead of accepting something it cannot honour.
func TestShareRejectsATimestamp(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv := u.server(t)

	rec, body := do(t, srv, http.MethodPost, "/v1/share/link",
		`{"path":"/Docs/report.pdf","expires_at":"2026-12-31T10:00:00Z"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	msg := body["error"].(map[string]any)["message"].(string)
	assertUserReadable(t, msg)
	if !strings.Contains(msg, "2026-12-31") {
		t.Errorf("the message does not show the shape wanted: %q", msg)
	}
}

func TestShareListAndRevoke(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv := u.server(t)

	_, body := do(t, srv, http.MethodGet, "/v1/share/list?path=/Docs/report.pdf", "")
	shares := body["shares"].([]any)
	if len(shares) != 1 {
		t.Fatalf("shares = %v", shares)
	}
	if shares[0].(map[string]any)["url"] != "https://od.lk/f/ABC" {
		t.Errorf("share = %v", shares[0])
	}

	rec, revoked := do(t, srv, http.MethodDelete, "/v1/share?path=/Docs/report.pdf", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(revoked["detail"].(string), "no longer works") {
		t.Errorf("detail = %v", revoked["detail"])
	}
}

func TestTrashListAndEmpty(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv := u.server(t)

	rec, _ := do(t, srv, http.MethodGet, "/v1/trash", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	rec2, body := do(t, srv, http.MethodPost, "/v1/trash/empty", "")
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d", rec2.Code)
	}
	assertUserReadable(t, body["detail"].(string))
}

func TestVersionsListsThem(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv := u.server(t)

	rec, body := do(t, srv, http.MethodGet, "/v1/versions?path=/Docs/report.pdf", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if len(body["versions"].([]any)) != 1 {
		t.Errorf("versions = %v", body["versions"])
	}
}

// The engine's error travels to the client as the classifier wrote it.
func TestAFailedJobReportsTheClassifiersMessage(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	// close_file_upload fails permanently.
	base := u.srv.Config.Handler
	u.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "close_file_upload") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":400,"message":"Invalid upload file size"}}`)
			return
		}
		base.ServeHTTP(w, r)
	})
	srv, _ := u.serverWithEngine(t)

	local := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(local, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, queued := do(t, srv, http.MethodPost, "/v1/upload",
		fmt.Sprintf(`{"local_path":%q,"remote_path":"/Docs/fail.bin"}`, local))
	job := waitJob(t, srv, queued["id"].(string))

	if job.State != jobs.StateFailed || job.Error == nil {
		t.Fatalf("job = %s %+v", job.State, job.Error)
	}
	if job.Error.Code != string(opendrive.KindInvalidRequest) {
		t.Errorf("code = %q, want the classifier's verdict", job.Error.Code)
	}
	// And the client sees the same thing through /v1/jobs/{id}.
	_, body := do(t, srv, http.MethodGet, "/v1/jobs/"+job.ID, "")
	errObj := body["error"].(map[string]any)
	if errObj["code"] != job.Error.Code {
		t.Errorf("the reported code drifted: %v vs %v", errObj["code"], job.Error.Code)
	}
}

// A failed job must reach the caller in the Bridge's words, not the
// classifier's. The engine records the diagnosis verbatim — evidence for whoever
// reads a log — and turning that into something a user can act on is this
// boundary's job. It was only being done for synchronous failures, and every
// upload and download is a job, so the most common failure a new user meets was
// the one that got the engineer's sentence.
func TestAFailedJobIsReportedInTheBridgesWords(t *testing.T) {
	diagnosis := "the credential is working and upstream refused this twice, so it is a real " +
		"restriction on this operation rather than a credential problem"
	job := &jobs.Job{
		ID: "j1", Kind: jobs.KindUpload, State: jobs.StateFailed,
		Error: &jobs.Error{
			Code: string(opendrive.KindUpstreamError), Message: diagnosis,
			Retryable: false, HTTPStatus: 403,
		},
	}
	s := &Server{}
	got := s.readable(job)

	if got.Error.Message == diagnosis {
		t.Fatal("the classifier's diagnosis reached the caller unchanged")
	}
	assertUserReadable(t, got.Error.Message)
	if got.Error.Retryable {
		t.Error("a real restriction was reported as retryable")
	}
	// The engine's own copy must not have been touched: it is still using it.
	if job.Error.Message != diagnosis {
		t.Error("the engine's job was mutated")
	}
}

// A job that failed for a reason the Bridge has no better wording for keeps the
// message it has. Replacing it with something vague would be worse.
func TestAFailedJobWithNoBetterWordingKeepsIts(t *testing.T) {
	job := &jobs.Job{
		ID: "j2", Kind: jobs.KindDownload, State: jobs.StateFailed,
		Error: &jobs.Error{Code: "quota_exceeded",
			Message: "Your OpenDrive account is out of storage space."},
	}
	s := &Server{}
	if got := s.readable(job).Error.Message; got != job.Error.Message {
		t.Errorf("message was rewritten to %q", got)
	}
}
