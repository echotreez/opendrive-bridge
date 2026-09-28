package odfake

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // upstream's hash
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// The fake is only worth having if it behaves like the real thing where the SDK
// depends on it. These tests drive it through the real SDK and check the quirks
// it claims to reproduce — a fake that quietly stopped reproducing one would
// turn every test built on it into a test of something else.

type auth struct{}

func (auth) Credentials(context.Context) (opendrive.Credentials, error) {
	return opendrive.Credentials{SessionID: "s"}, nil
}
func (auth) Refresh(context.Context) error { return nil }
func (auth) Identity() opendrive.Identity {
	return opendrive.Identity{Username: "t", AuthMode: opendrive.AuthModeOAuth2, Seamless: true}
}
func (auth) AuthState() opendrive.AuthState { return opendrive.StateAuthenticated }

func rig(t *testing.T) (*Server, *opendrive.Client, *httptest.Server) {
	t.Helper()
	fake := New()
	hs := httptest.NewServer(fake.Handler())
	t.Cleanup(hs.Close)
	c, err := opendrive.New(
		opendrive.WithBaseURL(hs.URL+"/api/v1"),
		opendrive.WithHTTPClient(hs.Client()),
		opendrive.WithAuthenticator(auth{}),
		opendrive.WithAccessProbe(nil),
		opendrive.WithRetryPolicy(opendrive.RetryPolicy{Max: 0}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return fake, c, hs
}

func TestUploadDownloadAndTheHashIsComputedNotEchoed(t *testing.T) {
	fake, c, _ := rig(t)
	ctx := context.Background()
	id, err := c.Folders().EnsurePath(ctx, "/a/b")
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("0123456789"), 300_000) // several chunks
	sum := md5.Sum(data)                                //nolint:gosec
	res, err := c.Uploads().Upload(ctx, bytes.NewReader(data), opendrive.UploadParams{
		FolderID: id, Name: "f.bin", Size: int64(len(data)), Hash: hex.EncodeToString(sum[:]), OpenIfExists: true,
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.File.FileHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash = %s", res.File.FileHash)
	}
	if got, ok := fake.File("a/b/f.bin"); !ok || !bytes.Equal(got, data) {
		t.Fatal("the fake does not hold the bytes")
	}

	fid, err := c.Files().IDByPath(ctx, "/a/b/f.bin")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := c.Downloads().Download(ctx, &buf, opendrive.DownloadParams{FileID: fid, Hash: res.File.FileHash}); err != nil {
		t.Fatalf("download: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Fatal("downloaded bytes differ")
	}
	buf.Reset()
	if _, err := c.Downloads().Download(ctx, &buf, opendrive.DownloadParams{FileID: fid, Offset: 5}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), data[5:]) {
		t.Fatal("a ranged download returned the wrong bytes")
	}

	// Overwriting in place with open_if_exists, and a refusal without it.
	small := []byte("new")
	if _, err := c.Uploads().Upload(ctx, bytes.NewReader(small), opendrive.UploadParams{
		FolderID: id, Name: "f.bin", Size: 3, OpenIfExists: true,
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := fake.File("a/b/f.bin"); string(got) != "new" {
		t.Fatalf("after overwrite: %q", got)
	}
}

// D35: TotalWritten is the chunk's size. D37: chunks in order only. D52: an
// unclosed record lists with size 0 and no hash.
func TestTheRecordedQuirks(t *testing.T) {
	fake, _, hs := rig(t)
	post := func(path, body string) (int, string) {
		resp, err := http.Post(hs.URL+"/api/v1"+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	_, out := post("/upload/create_file.json", `{"folder_id":"0","file_name":"q","file_size":6}`)
	if !strings.Contains(out, `"FileId":"FILE`) {
		t.Fatalf("create_file: %s", out)
	}
	id := strings.Split(strings.Split(out, `"FileId":"`)[1], `"`)[0]
	post("/upload/open_file_upload.json", fmt.Sprintf(`{"file_id":%q,"file_size":6}`, id))

	chunk := func(offset int, data string) (int, string) {
		var b bytes.Buffer
		b.WriteString("--X\r\nContent-Disposition: form-data; name=\"file_data\"; filename=\"c\"\r\n\r\n")
		b.WriteString(data)
		b.WriteString("\r\n--X--\r\n")
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/v1/upload/upload_file_chunk2.json/s/%s?chunk_offset=%d&chunk_size=%d",
			hs.URL, id, offset, len(data)), &b)
		req.Header.Set("Content-Type", "multipart/form-data; boundary=X")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if _, out := chunk(0, "abc"); !strings.Contains(out, `"TotalWritten":"3"`) {
		t.Fatalf("first chunk: %s", out)
	}
	if code, out := chunk(0, "abc"); code != 400 || !strings.Contains(out, "Incorrect chunk offset") {
		t.Fatalf("a repeated offset = %d %s", code, out)
	}
	if _, out := chunk(3, "def"); !strings.Contains(out, `"TotalWritten":"3"`) {
		t.Fatalf("second chunk reported a running total: %s", out)
	}
	// Listed before it is closed: size 0, no hash.
	resp, _ := http.Get(hs.URL + "/api/v1/folder/list.json/s/0")
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(b), `"Size":"0"`) || !strings.Contains(string(b), `"FileHash":""`) {
		t.Fatalf("an unclosed record: %s", b)
	}
	if code, out := post("/upload/close_file_upload.json", fmt.Sprintf(`{"file_id":%q,"file_size":7}`, id)); code != 400 {
		t.Fatalf("a close with the wrong size = %d %s", code, out)
	}
	if code, _ := post("/upload/close_file_upload.json", fmt.Sprintf(`{"file_id":%q,"file_size":6,"file_time":1700000000}`, id)); code != 200 {
		t.Fatal("close failed")
	}
	if got, _ := fake.File("q"); string(got) != "abcdef" {
		t.Fatalf("content = %q", got)
	}
	if code, _ := post("/nope.json", `{}`); code != 501 {
		t.Fatal("an unimplemented endpoint did not say so")
	}
}

func TestListingsPageAndFoldersAndTrash(t *testing.T) {
	fake, c, _ := rig(t)
	ctx := context.Background()
	for i := 0; i < 130; i++ {
		fake.PutFile(fmt.Sprintf("big/f%03d", i), []byte{byte(i)})
	}
	fake.Mkdir("big/sub")
	id, err := c.Folders().IDByPath(ctx, "/big")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	pager := c.Folders().Pages(id, opendrive.ListOptions{})
	for !pager.Done() {
		page, err := pager.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if page == nil {
			break
		}
		if page.Len() > 100 {
			t.Fatalf("a page of %d; upstream pages at 100", page.Len())
		}
		n += page.Len()
	}
	if n != 131 {
		t.Fatalf("paged through %d items, want 131", n)
	}
	only, err := c.Folders().List(ctx, id, opendrive.ListOptions{OnlySubfolders: true})
	if err != nil || len(only.Files) != 0 || len(only.Folders) != 1 {
		t.Fatalf("only_subfolders: %+v %v", only, err)
	}

	if _, err := c.Folders().Create(ctx, opendrive.CreateFolderParams{Name: "sub", ParentID: id}); err == nil {
		t.Fatal("a duplicate folder was created")
	}
	fid, _ := c.Files().IDByPath(ctx, "/big/f001")
	if err := c.Files().Trash(ctx, []string{fid}); err != nil {
		t.Fatal(err)
	}
	if !fake.Trashed("big/f001") {
		t.Fatal("not in the trash")
	}
	info, err := c.Files().Info(ctx, fid)
	if err != nil || info.DateTrashed.IsZero() {
		t.Fatalf("info of a trashed file: %+v %v", info, err)
	}
	if err := c.Files().Remove(ctx, []string{fid}, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Files().Info(ctx, fid); !errors.Is(err, opendrive.ErrNotFound) {
		t.Fatalf("info of a removed file = %v", err)
	}
	if err := c.Folders().Trash(ctx, []string{id}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Folders().IDByPath(ctx, "/big"); !errors.Is(err, opendrive.ErrNotFound) {
		t.Fatalf("a trashed folder still resolves: %v", err)
	}
	if err := c.Folders().Remove(ctx, []string{id}); err != nil {
		t.Fatal(err)
	}

	fake.SetDown(true)
	if _, err := c.Folders().IDByPath(ctx, "/anything"); err == nil || !opendrive.IsTemporary(err) {
		t.Fatalf("while down: %v", err)
	}
	fake.SetDown(false)
	if fake.Calls("folder/list") == 0 {
		t.Fatal("calls are not counted")
	}
}
