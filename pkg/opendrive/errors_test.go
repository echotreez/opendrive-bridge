package opendrive

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// §11: the three upstream error shapes must collapse into one APIError.
func TestParseErrorShapes(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantKind   Kind
		wantMsg    string
		wantOAuth  string
		wantUpCode int
	}{
		{
			name:       "rest object",
			status:     404,
			body:       `{"error":{"code":404,"message":"File not exists"}}`,
			wantKind:   KindNotFound,
			wantMsg:    "File not exists",
			wantUpCode: 404,
		},
		{
			name:       "oauth invalid_token on a rest call",
			status:     401,
			body:       `{"error":{"code":401,"error":"invalid_token","error_description":"The access token provided has expired"}}`,
			wantKind:   KindTokenExpired,
			wantMsg:    "The access token provided has expired",
			wantOAuth:  "invalid_token",
			wantUpCode: 401,
		},
		{
			name:      "oauth grant failure",
			status:    400,
			body:      `{"error":"invalid_grant","error_description":"Refresh token is invalid"}`,
			wantKind:  KindRefreshTokenFailed,
			wantMsg:   "Refresh token is invalid",
			wantOAuth: "invalid_grant",
		},
		{
			name:     "plain text body",
			status:   500,
			body:     "Internal Server Error",
			wantKind: KindUpstreamError,
			wantMsg:  "Internal Server Error",
		},
		{
			name:     "captcha",
			status:   403,
			body:     `{"error":{"code":403,"message":"Captcha required"}}`,
			wantKind: KindCaptchaRequired,
			wantMsg:  "Captcha required",
		},
		{
			name:     "bandwidth",
			status:   403,
			body:     `{"error":{"code":403,"message":"Bandwidth limit exceeded"}}`,
			wantKind: KindBandwidthExceeded,
		},
		{
			name:     "quota",
			status:   403,
			body:     `{"error":{"code":403,"message":"Storage limit reached"}}`,
			wantKind: KindQuotaExceeded,
		},
		{
			name:     "invalid name",
			status:   400,
			body:     `{"error":{"code":400,"message":"Invalid file name"}}`,
			wantKind: KindInvalidName,
		},
		{
			name:     "conflict",
			status:   409,
			body:     `{"error":{"code":409,"message":"File exists"}}`,
			wantKind: KindConflict,
		},
		{
			name:     "rate limited",
			status:   429,
			body:     `{"error":{"code":429,"message":"Too many requests"}}`,
			wantKind: KindRateLimited,
		},
		{
			// v1.1 §4.5: unauthorized is reserved for the Bridge API's own
			// auth, so an upstream refusal is a plain upstream error.
			name:     "forbidden is an upstream refusal, not a credential problem",
			status:   403,
			body:     `{"error":{"code":403,"message":"Access denied"}}`,
			wantKind: KindUpstreamError,
		},
		{
			// A 401 that is not about the credentials means the session or
			// token on the wire is stale, which the SDK renews silently.
			name:     "session expiry is renewable",
			status:   401,
			body:     `{"error":{"code":401,"message":"Session does not exist"}}`,
			wantKind: KindTokenExpired,
		},
		{
			// §2.2 #4a: the one case that must stop every automatic attempt.
			name:     "rejected password",
			status:   401,
			body:     `{"error":{"code":401,"message":"Invalid username or password"}}`,
			wantKind: KindReauthRequired,
		},
		{
			name:      "rejected oauth client",
			status:    401,
			body:      `{"error":"invalid_client","error_description":"Invalid username or password"}`,
			wantKind:  KindReauthRequired,
			wantOAuth: "invalid_client",
		},
		{
			name:     "suspended account",
			status:   403,
			body:     `{"error":{"code":403,"message":"Account is suspended"}}`,
			wantKind: KindReauthRequired,
		},
		{
			name:     "message only",
			status:   400,
			body:     `{"message":"bad request"}`,
			wantKind: KindUpstreamError,
			wantMsg:  "bad request",
		},
		{
			name:     "empty body",
			status:   500,
			body:     ``,
			wantKind: KindUpstreamError,
		},
		{
			name:     "payload too large is a quota problem",
			status:   413,
			body:     ``,
			wantKind: KindQuotaExceeded,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := parseError(tc.status, nil, []byte(tc.body), "GET /x.json", "https://x/x.json")
			if err.Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", err.Kind, tc.wantKind)
			}
			if tc.wantMsg != "" && err.UpstreamMsg != tc.wantMsg {
				t.Errorf("message = %q, want %q", err.UpstreamMsg, tc.wantMsg)
			}
			if tc.wantOAuth != "" && err.OAuthError != tc.wantOAuth {
				t.Errorf("oauth error = %q, want %q", err.OAuthError, tc.wantOAuth)
			}
			if tc.wantUpCode != 0 && err.UpstreamCode != tc.wantUpCode {
				t.Errorf("upstream code = %d, want %d", err.UpstreamCode, tc.wantUpCode)
			}
			if err.Error() == "" {
				t.Error("Error() is empty")
			}
		})
	}
}

func TestAPIErrorIsAndKind(t *testing.T) {
	err := parseError(404, nil, []byte(`{"error":{"code":404,"message":"File not exists"}}`), "GET /file/info.json", "https://x")
	if !errors.Is(err, ErrNotFound) {
		t.Fatal("errors.Is(err, ErrNotFound) = false")
	}
	if errors.Is(err, ErrConflict) {
		t.Fatal("errors.Is matched the wrong sentinel")
	}
	if errors.Is(err, errors.New("other")) {
		t.Fatal("errors.Is matched a foreign error")
	}
	if ErrorKind(err) != KindNotFound {
		t.Fatalf("ErrorKind = %q", ErrorKind(err))
	}
	if ErrorKind(errors.New("plain")) != "" {
		t.Fatal("ErrorKind of a plain error must be empty")
	}

	wrapped := &APIError{Kind: KindNetwork, Err: errors.New("connection reset")}
	if !errors.Is(wrapped, ErrNetwork) {
		t.Fatal("wrapped network error does not match ErrNetwork")
	}
	if wrapped.Unwrap() == nil {
		t.Fatal("Unwrap returned nil")
	}
	mustContain(t, wrapped.Error(), "connection reset", "wrapped error message")
}

// §11: only genuinely transient conditions may be retried.
func TestTemporaryClassification(t *testing.T) {
	temporary := []*APIError{
		{Kind: KindRateLimited},
		{Kind: KindNetwork},
		{Kind: KindUpstreamError, HTTPCode: 500},
		{Kind: KindUpstreamError, HTTPCode: 503},
	}
	for _, e := range temporary {
		if !e.Temporary() || !IsTemporary(e) {
			t.Errorf("%v should be temporary", e.Kind)
		}
	}
	permanent := []*APIError{
		{Kind: KindCaptchaRequired},
		{Kind: KindQuotaExceeded},
		{Kind: KindBandwidthExceeded},
		{Kind: KindInvalidName},
		{Kind: KindNotFound},
		{Kind: KindConflict},
		{Kind: KindUnauthorized},
		{Kind: KindTokenExpired},
		{Kind: KindRefreshTokenFailed},
		{Kind: KindUpstreamError, HTTPCode: 400},
	}
	for _, e := range permanent {
		if e.Temporary() {
			t.Errorf("%v must never be retried automatically", e.Kind)
		}
	}
	if IsTemporary(errors.New("plain")) {
		t.Error("a plain error is not temporary")
	}
}

func TestRetryAfterParsing(t *testing.T) {
	h := http.Header{"Retry-After": []string{"7"}}
	err := parseError(429, h, nil, "GET /x", "https://x")
	if err.RetryAfter() != 7*time.Second {
		t.Fatalf("RetryAfter = %v", err.RetryAfter())
	}
	if RetryAfter(err) != 7*time.Second {
		t.Fatalf("package RetryAfter = %v", RetryAfter(err))
	}

	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	err = parseError(503, http.Header{"Retry-After": []string{future}}, nil, "GET /x", "https://x")
	if d := err.RetryAfter(); d <= 0 || d > 31*time.Second {
		t.Fatalf("http-date RetryAfter = %v", d)
	}

	for _, v := range []string{"", "not a date", "-1"} {
		err = parseError(503, http.Header{"Retry-After": []string{v}}, nil, "GET /x", "https://x")
		if err.RetryAfter() != 0 {
			t.Fatalf("RetryAfter(%q) = %v, want 0", v, err.RetryAfter())
		}
	}
	if parseError(500, nil, nil, "GET /x", "https://x").RetryAfter() != 0 {
		t.Fatal("no header means no delay")
	}
	if RetryAfter(errors.New("plain")) != 0 {
		t.Fatal("a plain error carries no delay")
	}
}

func TestErrorMessageFormatting(t *testing.T) {
	e := &APIError{Kind: KindNotFound, Op: "GET /file/info.json", HTTPCode: 404, UpstreamCode: 4041,
		UpstreamMsg: "File not exists", OAuthError: "none"}
	msg := e.Error()
	mustContain(t, msg, "not_found", "kind")
	mustContain(t, msg, "GET /file/info.json", "op")
	mustContain(t, msg, "HTTP 404", "status")
	mustContain(t, msg, "upstream 4041", "upstream code")
	mustContain(t, msg, "File not exists", "message")

	bare := &APIError{Kind: KindNetwork}
	if bare.Error() != "network" {
		t.Fatalf("bare error = %q", bare.Error())
	}
}

func TestConstructorHelpers(t *testing.T) {
	if got := networkError("GET /x", "https://x", errors.New("boom")); got.Kind != KindNetwork {
		t.Errorf("networkError kind = %q", got.Kind)
	}
	if got := invalidResponse("GET /x", "https://x", errors.New("boom")); got.Kind != KindInvalidResponse {
		t.Errorf("invalidResponse kind = %q", got.Kind)
	}
	got := invalidRequest("bad %s", "thing")
	if got.Kind != KindInvalidRequest || got.UpstreamMsg != "bad thing" {
		t.Errorf("invalidRequest = %+v", got)
	}
}

func TestHelperFunctions(t *testing.T) {
	if firstNonEmpty("", "", "x", "y") != "x" {
		t.Error("firstNonEmpty")
	}
	if firstNonEmpty("", "") != "" {
		t.Error("firstNonEmpty of nothing")
	}
	if !isJSON([]byte(`  {"a":1}`)) || !isJSON([]byte(`[1]`)) || isJSON([]byte(`oops`)) || isJSON(nil) {
		t.Error("isJSON")
	}
	if truncate("abc", 5) != "abc" || truncate("abcdef", 3) != "abc..." {
		t.Error("truncate")
	}
}

// An error object nested at 200 with no HTTP status still has to classify.
func TestClassifyFallsBackToUpstreamCode(t *testing.T) {
	err := parseError(0, nil, []byte(`{"error":{"code":404,"message":"File not exists"}}`), "POST /x", "https://x")
	if err.Kind != KindNotFound {
		t.Fatalf("kind = %q, want not_found", err.Kind)
	}
}
