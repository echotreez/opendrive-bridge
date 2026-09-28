package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// §3.6.4, the old open question #9: a write-back write is accepted while
// OpenDrive is unreachable, because that is the moment a client most needs the
// gateway to hold it.

func putStream(srv *Server, path string, content []byte, overwrite bool) *httptest.ResponseRecorder {
	url := "/v1/upload/stream?path=" + path
	if overwrite {
		url += "&overwrite=true"
	}
	r := httptest.NewRequest(http.MethodPut, url, bytes.NewReader(content))
	r.ContentLength = int64(len(content))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	return rec
}

func listNames(t *testing.T, srv *Server, path string) (int, map[string]map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/ls?path="+path, nil))
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	var out struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	names := map[string]map[string]any{}
	for _, e := range out.Entries {
		names[e["name"].(string)] = e
	}
	return rec.Code, names
}

func TestAWriteDuringAnOutageIsAcceptedAndListed(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv, _ := u.serverWithEngine(t)
	_, held := withCache(t, srv, true)
	defer close(held.release)

	u.setDown(true)
	content := []byte("written while OpenDrive was away")
	rec := putStream(srv, "/Docs/new/deep/file.bin", content, true)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("a write during an outage = %d, want 202: %s", rec.Code, rec.Body.String())
	}

	// Readable at once.
	get := httptest.NewRecorder()
	srv.Handler().ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v1/download/stream?path=/Docs/new/deep/file.bin", nil))
	if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), content) {
		t.Fatalf("reading it back during the outage = %d", get.Code)
	}

	// Listed, down the folders that do not exist upstream yet, marked pending.
	for dir, name := range map[string]string{"/Docs/new": "deep", "/Docs/new/deep": "file.bin"} {
		code, names := listNames(t, srv, dir)
		if code != http.StatusOK {
			t.Fatalf("listing %s during the outage = %d", dir, code)
		}
		e, ok := names[name]
		if !ok {
			t.Fatalf("%s is not listed in %s: %v", name, dir, names)
		}
		if e["pending"] != true {
			t.Errorf("%s/%s is not marked pending: %v", dir, name, e)
		}
	}
}

// "Do not overwrite" is a promise about the destination that cannot be kept
// without asking OpenDrive, so it is refused in words rather than guessed at.
func TestANoOverwriteWriteDuringAnOutageIsRefusedInWords(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv, _ := u.serverWithEngine(t)
	_, held := withCache(t, srv, true)
	defer close(held.release)

	u.setDown(true)
	rec := putStream(srv, "/Docs/careful.bin", []byte("x"), false)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"cannot be reached", "overwrite=true"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the refusal does not say %q: %s", want, rec.Body.String())
		}
	}
}

// With OpenDrive up, a folder that does not exist is still a 404: accepting it
// would create folders nobody asked for.
func TestAMissingFolderIsStillRefusedWhenOpenDriveIsUp(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv, _ := u.serverWithEngine(t)
	_, held := withCache(t, srv, true)
	defer close(held.release)

	rec := putStream(srv, "/Nope/file.bin", []byte("x"), true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// A file upstream already lists and the bridge holds a newer copy of appears
// once, as the pending one.
func TestAPendingOverwriteIsListedOnce(t *testing.T) {
	u := newFakeUpstream(t)
	u.withTransfers(nil)
	srv, _ := u.serverWithEngine(t)
	_, held := withCache(t, srv, true)
	defer close(held.release)

	rec := putStream(srv, "/Docs/report.pdf", []byte("a newer report"), true)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	code, names := listNames(t, srv, "/Docs")
	if code != http.StatusOK {
		t.Fatalf("ls = %d", code)
	}
	e := names["report.pdf"]
	if e == nil || e["pending"] != true || int(e["size"].(float64)) != len("a newer report") {
		t.Fatalf("report.pdf = %v; want the pending copy", e)
	}
}
