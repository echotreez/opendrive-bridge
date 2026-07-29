package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/StormRealm/opendrive-bridge/pkg/opendrive"
)

// This file is the Bridge boundary, and the boundary has one job: **upstream's
// lies stop here.**
//
// Forty-four discrepancies are recorded in docs/discrepancies.md, and a good
// number of them are not bugs to route around but *statements upstream makes
// that are not true*. A 403 that says "permission" for a transient refusal, a
// 200 with an empty archive, a 404 for a file that was just uploaded. Every one
// of them, left alone, becomes a sentence a user reads and acts on wrongly.
//
// So the rule for every message written here is a question: **would someone who
// has never heard of the OpenDrive API know what to do after reading this?** If
// the answer is no, the message is not finished. That rules out passing
// upstream's wording through, and it also rules out replacing it with a code
// name — "upstream_error" tells a user nothing either.

// Error is the response envelope of whitepaper §4.5.
type Error struct {
	// Code is the stable machine-readable enum, straight from the
	// classification layer. Clients switch on this.
	Code string `json:"code"`
	// HTTP is the status the Bridge answered with.
	HTTP int `json:"http"`
	// Message is for a person. It says what happened and, where there is one,
	// what to do about it. It never contains a token, a local path or an
	// upstream identifier.
	Message string `json:"message"`
	// Upstream is the raw detail, for operators and bug reports. It is
	// deliberately a separate field so that a client showing `message` cannot
	// accidentally show upstream's misleading text instead.
	Upstream *UpstreamDetail `json:"upstream,omitempty"`
}

// UpstreamDetail is what upstream actually said, kept for diagnosis.
type UpstreamDetail struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type errorEnvelope struct {
	Error Error `json:"error"`
}

// statusFor maps a classification Kind onto an HTTP status for the Bridge's own
// response. It never reuses upstream's status: upstream's 403 for a transient
// refusal would tell a client to stop trying, and its 404 for a file that
// exists would tell a client the file is gone.
func statusFor(kind opendrive.Kind) int {
	switch kind {
	case opendrive.KindNotFound:
		return http.StatusNotFound
	case opendrive.KindConflict:
		return http.StatusConflict
	case opendrive.KindInvalidName, opendrive.KindInvalidRequest:
		return http.StatusBadRequest
	case opendrive.KindUnauthorized:
		return http.StatusUnauthorized
	case opendrive.KindReauthRequired, opendrive.KindCaptchaRequired:
		// The bridge cannot proceed without the user, and the user can act:
		// 428 says "there is a precondition you must satisfy".
		return http.StatusPreconditionRequired
	case opendrive.KindKeystoreUnavailable:
		return http.StatusServiceUnavailable
	case opendrive.KindQuotaExceeded:
		return http.StatusInsufficientStorage
	case opendrive.KindBandwidthExceeded, opendrive.KindRateLimited:
		return http.StatusTooManyRequests
	case opendrive.KindTokenExpired, opendrive.KindRefreshTokenFailed:
		// Transient and handled internally; if one escapes, it is worth
		// retrying rather than reporting as a failure of the request.
		return http.StatusServiceUnavailable
	case opendrive.KindNetwork, opendrive.KindEdgeRejected:
		return http.StatusBadGateway
	case opendrive.KindInvalidResponse:
		return http.StatusBadGateway
	default:
		return http.StatusBadGateway
	}
}

// messages are the user-facing sentences, one per Kind.
//
// Each says what happened in terms of the user's own world — their file, their
// account, their password — and what to do next. None of them mentions
// OpenDrive's API, an endpoint, or a status code, because a user cannot act on
// any of those.
var messages = map[opendrive.Kind]string{
	opendrive.KindKeystoreUnavailable: "The bridge cannot reach its credential store, so it will not try to " +
		"sign in. Unlock your login keychain and the bridge will recover on its own.",
	opendrive.KindReauthRequired: "Your saved password is no longer accepted. Sign in again with your " +
		"current password to let the bridge resume.",
	opendrive.KindCaptchaRequired: "OpenDrive is asking for a captcha, which the bridge cannot answer. " +
		"Sign in once at opendrive.com to clear it, then try again.",
	opendrive.KindTokenExpired: "The bridge's sign-in expired while handling this request. " +
		"Try again in a moment.",
	opendrive.KindRefreshTokenFailed: "The bridge's sign-in expired while handling this request. " +
		"Try again in a moment.",
	opendrive.KindUnauthorized: "This request needs a valid bridge API key.",
	opendrive.KindNotFound:     "There is nothing at that path.",
	opendrive.KindConflict:     "Something already exists at that path. Choose another name, or allow overwriting.",
	opendrive.KindQuotaExceeded: "Your OpenDrive account is out of storage space. Free some up, or upgrade " +
		"the plan, then try again.",
	opendrive.KindBandwidthExceeded: "Your OpenDrive account has used up its download allowance for now. " +
		"It resets on your account's own schedule; retrying sooner will not help.",
	opendrive.KindInvalidName: "That name contains characters OpenDrive will not accept. " +
		"Avoid \\ / : * ? \" < > | and trailing spaces or dots.",
	opendrive.KindRateLimited: "OpenDrive is asking the bridge to slow down. The request will work " +
		"if you try again shortly.",
	opendrive.KindNetwork: "The bridge could not reach OpenDrive. Check your network connection and " +
		"try again.",
	opendrive.KindEdgeRejected: "OpenDrive's servers turned the request away before it reached them " +
		"properly. This is usually temporary — try again shortly.",
	opendrive.KindInvalidResponse: "OpenDrive returned something the bridge could not make sense of. " +
		"The request was not completed; nothing has been changed.",
	opendrive.KindInvalidRequest: "The bridge could not carry out that request as asked.",
	opendrive.KindUpstreamError:  "OpenDrive refused the request.",
}

// WriteError renders err as the §4.5 envelope.
//
// The message is chosen in a strict order, and the order is the point:
//
//  1. the classification layer's Diagnosis, when it has one. That is exactly
//     the case where upstream's own wording is known to be misleading — the
//     permission-shaped 403 of D39/D40 above all — so it outranks everything.
//  2. a message written here for the Kind.
//  3. a last-resort sentence. Upstream's text is never promoted to `message`;
//     it travels in `upstream` where a client will not mistake it for advice.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	env := errorEnvelope{Error: buildError(err)}

	if log := loggerFrom(r.Context()); log != nil {
		log.Warn("request failed",
			slog.String("code", env.Error.Code),
			slog.Int("http", env.Error.HTTP),
			slog.String("error", opendrive.RedactString(err.Error())))
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(env.Error.HTTP)
	_ = json.NewEncoder(w).Encode(env)
}

func buildError(err error) Error {
	if err == nil {
		return Error{Code: string(opendrive.KindUpstreamError), HTTP: http.StatusInternalServerError,
			Message: messages[opendrive.KindUpstreamError]}
	}

	// A Bridge-level problem the user caused: it has its own message already,
	// written for them, and no upstream involvement.
	var re *RequestError
	if errors.As(err, &re) {
		return Error{Code: re.Code, HTTP: re.HTTP, Message: re.Message}
	}

	kind := opendrive.ErrorKind(err)
	if kind == "" {
		kind = opendrive.KindUpstreamError
	}
	out := Error{Code: string(kind), HTTP: statusFor(kind)}

	var ae *opendrive.APIError
	if errors.As(err, &ae) {
		// The classifier speaks up precisely when upstream's message misleads.
		if d := ae.Diagnosis(); d != "" {
			out.Message = userWordingFor(kind, d)
		}
		if ae.UpstreamMsg != "" || ae.HTTPCode != 0 {
			out.Upstream = &UpstreamDetail{
				Code:    ae.HTTPCode,
				Message: opendrive.RedactString(ae.UpstreamMsg),
			}
		}
	}
	if out.Message == "" {
		out.Message = messages[kind]
	}
	if out.Message == "" {
		out.Message = "The bridge could not complete that request."
	}
	return out
}

// userWordingFor turns a classifier diagnosis into something a user can act on.
//
// The diagnosis is written for an engineer reading a log — "the credential is
// working and this same call has already succeeded on this resource" — which is
// true and useful but not what a person waiting on a file upload needs to read.
// The distinction the diagnosis draws *is* what matters, so it is preserved:
// a refusal the classifier judged transient is described as temporary, and one
// it confirmed is described as a real restriction. Neither is described as
// "permission denied", which is the wording upstream uses for both.
func userWordingFor(kind opendrive.Kind, diagnosis string) string {
	if kind != opendrive.KindUpstreamError {
		return ""
	}
	if containsAny(diagnosis, "transient refusal", "attempted once more") {
		return "OpenDrive turned this request away for a moment even though your account has " +
			"the right to do it. This clears up on its own — the bridge is retrying, and " +
			"trying again shortly will work."
	}
	if containsAny(diagnosis, "real restriction", "restriction on this operation") {
		return "Your OpenDrive account is not allowed to do this here. If you expect to be, " +
			"the account's administrator controls it."
	}
	if containsAny(diagnosis, "refusing this credential") {
		return "OpenDrive is no longer accepting the bridge's saved sign-in. " +
			"Sign in again with your current password."
	}
	return ""
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if n != "" && strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// RequestError is a problem with the request itself rather than with upstream:
// a malformed body, a missing parameter, a path the bridge will not accept. It
// carries its own user-facing message because only the handler knows what was
// wrong.
type RequestError struct {
	Code    string
	HTTP    int
	Message string
}

func (e *RequestError) Error() string { return e.Message }

// BadRequest builds a 400 with a message meant for a person.
func BadRequest(message string) *RequestError {
	return &RequestError{Code: string(opendrive.KindInvalidRequest), HTTP: http.StatusBadRequest,
		Message: message}
}

// Unauthorized builds the Bridge's own authentication failure. It is separate
// from every upstream credential kind on purpose (§4.5): this is about the
// bridge's API key, not about the user's OpenDrive password.
func Unauthorized(message string) *RequestError {
	return &RequestError{Code: string(opendrive.KindUnauthorized), HTTP: http.StatusUnauthorized,
		Message: message}
}
