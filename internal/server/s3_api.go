package server

import (
	"net/http"

	"github.com/echotreez/opendrive-bridge/internal/keystore"
	"github.com/echotreez/opendrive-bridge/pkg/opendrive"
)

// s3StatusResponse is GET /v1/s3. It carries the secret key: this API is the
// only way anybody learns it (it is never logged), and it is behind the same
// loopback address and optional API key as everything else that can read and
// delete the account's files.
type s3StatusResponse struct {
	Enabled bool   `json:"enabled"`
	Detail  string `json:"detail,omitempty"`
	S3Credentials
	S3Info
	Region    string `json:"region,omitempty"`
	PathStyle bool   `json:"path_style,omitempty"`
}

func (s *Server) handleS3Status(w http.ResponseWriter, r *http.Request) {
	g := s.s3.Load()
	if g == nil {
		writeJSON(w, r, http.StatusOK, s3StatusResponse{
			Enabled: false,
			Detail: "The S3 gateway is off. It runs when the caching gateway is on with write-back " +
				"(--cache-dir) and --s3 is not false.",
		})
		return
	}
	writeJSON(w, r, http.StatusOK, s3StatusResponse{
		Enabled:       true,
		S3Credentials: g.Credentials(),
		S3Info:        g.Info(),
		// Any region is accepted in the signature; clients that insist on one
		// should be given this.
		Region:    "us-east-1",
		PathStyle: true,
	})
}

func (s *Server) handleS3Reset(w http.ResponseWriter, r *http.Request) {
	g := s.s3.Load()
	if g == nil {
		WriteError(w, r, &RequestError{Code: string(opendrive.KindInvalidRequest), HTTP: http.StatusConflict,
			Message: "The S3 gateway is off, so it has no keys to reset."})
		return
	}
	sec, ok := s.store.(keystore.Secrets)
	if !ok {
		WriteError(w, r, &RequestError{Code: string(opendrive.KindKeystoreUnavailable),
			HTTP: http.StatusServiceUnavailable, Message: "The credential store cannot hold the S3 keys."})
		return
	}
	c, err := ResetS3Credentials(r.Context(), sec)
	if err != nil {
		WriteError(w, r, &RequestError{Code: string(opendrive.KindKeystoreUnavailable),
			HTTP:    http.StatusServiceUnavailable,
			Message: "The new keys could not be saved, so the old ones still work: " + err.Error()})
		return
	}
	g.SetCredentials(c)
	s.log.Info("the S3 access key was reset; clients using the old one will be refused")
	writeJSON(w, r, http.StatusOK, s3StatusResponse{
		Enabled: true, S3Credentials: c, S3Info: g.Info(), Region: "us-east-1", PathStyle: true,
		Detail: "New keys are in use. Every client set up with the old ones must be given these.",
	})
}
