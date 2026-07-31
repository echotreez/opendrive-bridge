// Package cli is odctl: the command line over the Bridge (whitepaper §4.6).
//
// Two rules run through all of it.
//
// **The Bridge's message is the message.** The daemon already decided what a
// person should read — that is what `internal/server` exists for — so odctl
// prints `error.message` unchanged. Rewording it here would produce two
// vocabularies for the same failure and one of them would drift.
//
// **The exit code is part of the output.** Somebody putting odctl in a script
// needs to branch on what went wrong without parsing prose, so the codes
// separate "you typed something wrong" from "sign in again" from "OpenDrive is
// having a bad day".
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Exit codes. They are documented in the root command's help, because a code
// nobody can look up is no better than 1.
const (
	// ExitOK is success.
	ExitOK = 0
	// ExitUsage means the request was wrong: a bad path, a missing argument, a
	// name OpenDrive will not accept. Fix the command and run it again.
	ExitUsage = 2
	// ExitAuth means the bridge needs a person: the password changed, a captcha
	// is waiting, or the credential store is locked. A script should stop and
	// alert rather than retry.
	ExitAuth = 3
	// ExitUpstream means OpenDrive failed. Retrying later is reasonable.
	ExitUpstream = 4
	// ExitNotFound means the path does not exist. Separate from usage because a
	// script often wants to treat "not there" as an ordinary outcome.
	ExitNotFound = 5
	// ExitUnavailable means the daemon could not be reached at all.
	ExitUnavailable = 6
	// ExitRefused means OpenDrive refused this and will refuse it again: an
	// account that is not allowed to write where it was asked to, a quota that
	// is full. Separate from ExitUpstream because that one tells a script to
	// come back later, and coming back later will not help here.
	ExitRefused = 7
)

// APIError is a failure the Bridge reported, carrying its own wording.
type APIError struct {
	Code    string `json:"code"`
	HTTP    int    `json:"http"`
	Message string `json:"message"`
	// Retryable is the Bridge's verdict on whether trying again could work. A
	// pointer because absent and false are different: an endpoint that does not
	// say leaves the decision to the code, while an explicit false is the
	// classification layer stating that this will fail the same way next time.
	Retryable *bool `json:"retryable,omitempty"`
	// Upstream is kept for --verbose; it is never printed by default, because
	// it holds the wording the Bridge deliberately did not show.
	Upstream *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"upstream,omitempty"`
}

func (e *APIError) Error() string { return e.Message }

// ExitCode maps the Bridge's stable code onto a shell exit status.
func (e *APIError) ExitCode() int {
	switch e.Code {
	case "not_found":
		return ExitNotFound
	case "invalid_request", "invalid_name", "conflict":
		return ExitUsage
	case "reauth_required", "captcha_required", "keystore_unavailable", "unauthorized":
		return ExitAuth
	default:
		// The Bridge already decided whether this can be retried, using evidence
		// the CLI does not have. Ignoring that and returning ExitUpstream — "try
		// later" — for a permanent refusal tells a script to loop forever on a
		// folder its account will never be allowed to write to. Found by
		// following docs/first-run.md on such an account.
		if e.Retryable != nil && !*e.Retryable {
			return ExitRefused
		}
		return ExitUpstream
	}
}

// UnreachableError means the daemon is not answering.
type UnreachableError struct {
	Addr string
	Err  error
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("No bridge is answering at %s.\n"+
		"Start one with 'odctl daemon start', or point at another with --addr.", e.Addr)
}

func (e *UnreachableError) Unwrap() error { return e.Err }

// ExitCode implements the coded-error interface.
func (e *UnreachableError) ExitCode() int { return ExitUnavailable }

// Client talks to the Bridge.
type Client struct {
	base   string
	apiKey string
	hc     *http.Client
}

// NewClient builds a client for a bridge address.
func NewClient(addr, apiKey string, timeout time.Duration) *Client {
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	if timeout <= 0 {
		// Long by default: an upload of a large file is one request.
		timeout = 6 * time.Hour
	}
	return &Client{
		base:   strings.TrimRight(addr, "/"),
		apiKey: apiKey,
		hc:     &http.Client{Timeout: timeout},
	}
}

// Do performs a request and decodes the JSON answer into out.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return c.unreachable(err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return c.unreachable(readErr)
	}

	if resp.StatusCode >= 400 {
		return decodeAPIError(raw, resp.StatusCode)
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("the bridge sent something odctl could not read: %w", err)
		}
	}
	return nil
}

func (c *Client) unreachable(err error) error {
	var netErr net.Error
	if _, ok := err.(net.Error); ok || isConnRefused(err) {
		_ = netErr
		return &UnreachableError{Addr: c.base, Err: err}
	}
	return &UnreachableError{Addr: c.base, Err: err}
}

func isConnRefused(err error) bool {
	return err != nil && strings.Contains(err.Error(), "connection refused")
}

// decodeAPIError reads the Bridge's envelope. A response that is not one is
// still reported honestly rather than guessed at.
func decodeAPIError(raw []byte, status int) error {
	var env struct {
		Error *APIError `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Error != nil && env.Error.Message != "" {
		if env.Error.HTTP == 0 {
			env.Error.HTTP = status
		}
		return env.Error
	}
	return &APIError{
		Code: "upstream_error", HTTP: status,
		Message: fmt.Sprintf("The bridge answered %d with something odctl could not read.", status),
	}
}

// query builds a path with query parameters.
func query(path string, kv ...string) string {
	if len(kv) < 2 {
		return path
	}
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			v.Set(kv[i], kv[i+1])
		}
	}
	if len(v) == 0 {
		return path
	}
	return path + "?" + v.Encode()
}
