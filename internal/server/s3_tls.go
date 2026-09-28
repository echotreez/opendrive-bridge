package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The S3 port's certificate (§3.6.5). Synology Hyper Backup will only talk S3
// over TLS, and a NAS has no way to reach a certificate authority for a
// machine on the LAN, so the gateway makes its own the first time it starts.
// A user with a real certificate (DSM's Let's Encrypt one, say) points the
// bridge at it instead.
//
// The certificate is valid for 825 days, the longest some clients accept for a
// server certificate, and is replaced 30 days before it runs out. It names
// localhost, this machine's host name and every address it has when the
// certificate is made, plus any names given with --s3-tls-hosts. Its SHA-256
// fingerprint is logged and shown by /v1/s3, so that a person accepting it on
// a NAS can check they are accepting this one.

const (
	selfSignedValidity = 825 * 24 * time.Hour
	selfSignedRenew    = 30 * 24 * time.Hour
)

// S3TLS loads the certificate for the S3 port. With certFile and keyFile it
// uses those; otherwise it loads — or makes — a self-signed one in dir.
func S3TLS(dir, certFile, keyFile string, extraHosts []string) (*tls.Config, string, bool, error) {
	if (certFile == "") != (keyFile == "") {
		return nil, "", false, errors.New("give both --s3-tls-cert and --s3-tls-key, or neither")
	}
	custom := certFile != ""
	if !custom {
		certFile = filepath.Join(dir, "s3-cert.pem")
		keyFile = filepath.Join(dir, "s3-key.pem")
		if err := ensureSelfSigned(certFile, keyFile, extraHosts, time.Now()); err != nil {
			return nil, "", false, err
		}
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, "", custom, fmt.Errorf("cannot load the S3 certificate %s: %w", certFile, err)
	}
	sum := sha256.Sum256(pair.Certificate[0])
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{pair},
	}
	return cfg, fingerprint(sum[:]), custom, nil
}

func fingerprint(b []byte) string {
	h := strings.ToUpper(hex.EncodeToString(b))
	parts := make([]string, 0, len(h)/2)
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

// ensureSelfSigned leaves a usable certificate in place and makes one otherwise.
func ensureSelfSigned(certFile, keyFile string, extraHosts []string, now time.Time) error {
	if raw, err := os.ReadFile(certFile); err == nil { // #nosec G304 -- the bridge's own file
		if block, _ := pem.Decode(raw); block != nil {
			if cert, err := x509.ParseCertificate(block.Bytes); err == nil &&
				now.Add(selfSignedRenew).Before(cert.NotAfter) && coversHosts(cert, extraHosts) {
				if _, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
					return nil
				}
			}
		}
	}
	return makeSelfSigned(certFile, keyFile, extraHosts, now)
}

func coversHosts(cert *x509.Certificate, hosts []string) bool {
	for _, h := range hosts {
		if cert.VerifyHostname(h) != nil {
			return false
		}
	}
	return true
}

func makeSelfSigned(certFile, keyFile string, extraHosts []string, now time.Time) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	if err != nil {
		return err
	}
	hostName, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "opendrive-bridge S3 gateway", Organization: []string{"opendrive-bridge"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(selfSignedValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		// Marked as its own authority so that a NAS or a Mac can be told to
		// trust it, which some only allow for a CA certificate.
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	names := map[string]bool{}
	addName := func(h string) {
		h = strings.TrimSpace(h)
		if h == "" || names[h] {
			return
		}
		names[h] = true
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			return
		}
		tmpl.DNSNames = append(tmpl.DNSNames, h)
	}
	addName("localhost")
	addName("127.0.0.1")
	addName("::1")
	if hostName != "" {
		addName(hostName)
		if !strings.Contains(hostName, ".") {
			addName(hostName + ".local")
		}
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLinkLocalUnicast() {
				addName(ipn.IP.String())
			}
		}
	}
	for _, h := range extraHosts {
		addName(h)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(certFile), 0o700); err != nil {
		return err
	}
	// The key first, so that a certificate on disk always has its key beside it.
	if err := writeFileAtomic0600(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})); err != nil {
		return err
	}
	return writeFileAtomic0600(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func writeFileAtomic0600(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}
