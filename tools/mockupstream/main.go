// mockupstream is a stand-in for OpenDrive.com, used by the release smoke test.
//
// A release smoke test has one job: prove the binary that was just built
// actually runs on its target platform and can complete a request end to end.
// Pointing it at the real OpenDrive would make that test depend on somebody's
// account and on the network, and would say nothing extra about the binary.
//
// It answers the handful of endpoints a login and a listing touch, and it
// answers them the way the live API does — including the details the SDK was
// built around, because a mock that is politer than the real thing tests
// nothing. In particular: idbypath takes the path without a leading slash
// (D22), the listing splits folders and files into separate arrays, and numbers
// arrive as strings (D19).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address")
	portFile := flag.String("port-file", "", "write the chosen address here")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/", handle)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("mockupstream: %v", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if *portFile != "" {
		if err := os.WriteFile(*portFile, []byte(ln.Addr().String()), 0o600); err != nil {
			log.Fatalf("mockupstream: %v", err)
		}
	}
	fmt.Println("mockupstream listening on", ln.Addr().String())
	log.Fatal(srv.Serve(ln))
}

func handle(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{}
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
	}
	path := r.URL.Path
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	switch {
	case strings.Contains(path, "oauth2/grant"):
		// The token lifetimes are the ones the PDF documents; the SDK prefers
		// expires_in when it is present.
		_, _ = io.WriteString(w, `{"access_token":"mock-access","refresh_token":"mock-refresh",
			"expires_in":86400,"token_type":"Bearer"}`)

	case strings.Contains(path, "session/login"):
		_, _ = io.WriteString(w, `{"SessionID":"mock-session","UserID":"1","AccType":"1",
			"UserName":"smoke@example.com","FVersioning":"0"}`)

	case strings.Contains(path, "session/logout"):
		_, _ = io.WriteString(w, `{"result":true}`)

	case strings.Contains(path, "users/info"):
		// Strings for numbers (D19) and megabytes for the limits (D45): a mock
		// that normalises these would hide the code that copes with them.
		_, _ = io.WriteString(w, `{"UserID":1,"AccType":"1","UserName":"smoke@example.com",
			"StorageUsed":"1048576","MaxStorage":"1024","BwUsed":"0","BwMax":"10240",
			"AccessUserID":1}`)

	case strings.Contains(path, "folder/idbypath"):
		want, _ := body["path"].(string)
		// The live API takes the path with no leading slash (D22).
		switch strings.Trim(want, "/") {
		case "":
			_, _ = io.WriteString(w, `{"FolderId":"0"}`)
		case "Smoke":
			_, _ = io.WriteString(w, `{"FolderId":"FSMOKE"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":404,"message":"Folder not found"}}`)
		}

	case strings.Contains(path, "folder/list"):
		segs := strings.Split(strings.Trim(path, "/"), "/")
		switch segs[len(segs)-1] {
		case "FSMOKE":
			_, _ = io.WriteString(w, `{"DirUpdateTime":1785000002,"Folders":[],
				"Files":[{"FileId":"FILE1","Name":"hello.txt","Size":"11",
				"DateModified":1785000000}]}`)
		default:
			_, _ = io.WriteString(w, `{"DirUpdateTime":1785000001,
				"Folders":[{"FolderID":"FSMOKE","Name":"Smoke","DateModified":1785000000}],
				"Files":[]}`)
		}

	case strings.Contains(path, "folder.json") && r.Method == http.MethodPost:
		_, _ = io.WriteString(w, `{"FolderID":"FNEW","Name":"created"}`)

	default:
		_, _ = io.WriteString(w, `{}`)
	}
}
