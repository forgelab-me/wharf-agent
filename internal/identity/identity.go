// Package identity generates and persists the self-signed TLS identity
// used for mTLS enrollment — cf. ARCHITECTURE.md, "Enrôlement d'un nouvel
// agent". No internal CA: trust is established by pinning the SHA-256
// fingerprint of a self-signed certificate, not by chain validation.
//
// Deliberately duplicated between server/ and agent/ rather than shared:
// the two Go modules stay independent ahead of the planned split into two
// repos (cf. ARCHITECTURE.md, "Contexte / lignée").
package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	certFileName = "identity.cert.pem"
	keyFileName  = "identity.key.pem"
)

// LoadOrGenerate loads an existing identity from dir, or generates and
// persists a fresh self-signed one if none exists yet. The private key
// never leaves this process except serialized to its own 0600 file.
func LoadOrGenerate(dir string) (tls.Certificate, error) {
	certPath := filepath.Join(dir, certFileName)
	keyPath := filepath.Join(dir, keyFileName)

	if _, err := os.Stat(certPath); err == nil {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			// A rotation interrupted between its two renames leaves the
			// key replaced but not the certificate: finish it if the
			// prepared pair is intact.
			if c, cerr := finishInterrupted(dir); cerr == nil {
				return c, nil
			}
			return tls.Certificate{}, fmt.Errorf("load existing identity: %w", err)
		}
		return cert, nil
	}

	return generate(dir, certPath, keyPath)
}

const (
	nextCertFileName = "identity.next.cert.pem"
	nextKeyFileName  = "identity.next.key.pem"
)

// Age is how long ago the certificate's validity started -- the rotation
// policy keys on it (an agent enrolled years ago gets rotated too).
func Age(cert tls.Certificate) time.Duration {
	if len(cert.Certificate) == 0 {
		return 0
	}
	c, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return 0
	}
	return time.Since(c.NotBefore)
}

// PrepareNext returns the certificate this agent will rotate to, generating
// and persisting it (separate files, current identity untouched) if no
// prepared pair exists yet. Reusing an existing pair makes the whole
// rotation retryable after a crash.
func PrepareNext(dir string) (tls.Certificate, error) {
	certPath := filepath.Join(dir, nextCertFileName)
	keyPath := filepath.Join(dir, nextKeyFileName)
	if _, err := os.Stat(certPath); err == nil {
		if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
			return cert, nil
		}
	}
	return generate(dir, certPath, keyPath)
}

// CommitNext makes the prepared pair the agent's identity. Key first, then
// certificate: LoadOrGenerate recognises the in-between state and finishes
// the job.
func CommitNext(dir string) (tls.Certificate, error) {
	nextCert := filepath.Join(dir, nextCertFileName)
	nextKey := filepath.Join(dir, nextKeyFileName)
	cert, err := tls.LoadX509KeyPair(nextCert, nextKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load prepared identity: %w", err)
	}
	if err := os.Rename(nextKey, filepath.Join(dir, keyFileName)); err != nil {
		return tls.Certificate{}, fmt.Errorf("install key: %w", err)
	}
	if err := os.Rename(nextCert, filepath.Join(dir, certFileName)); err != nil {
		return tls.Certificate{}, fmt.Errorf("install certificate: %w", err)
	}
	return cert, nil
}

// finishInterrupted recovers from a CommitNext that died between its two
// renames: the new key is already installed, the matching new certificate
// is still under its prepared name.
func finishInterrupted(dir string) (tls.Certificate, error) {
	nextCert := filepath.Join(dir, nextCertFileName)
	keyPath := filepath.Join(dir, keyFileName)
	cert, err := tls.LoadX509KeyPair(nextCert, keyPath)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.Rename(nextCert, filepath.Join(dir, certFileName)); err != nil {
		return tls.Certificate{}, err
	}
	return cert, nil
}

func generate(dir, certPath, keyPath string) (tls.Certificate, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, fmt.Errorf("create identity dir: %w", err)
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate serial: %w", err)
	}

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "wharf"
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostname},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{hostname},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create certificate: %w", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("marshal key: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("write cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("write key: %w", err)
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load generated identity: %w", err)
	}
	return cert, nil
}

// Fingerprint returns the SHA-256 fingerprint of a DER-encoded
// certificate, formatted as "sha256:AA:BB:...".
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return "sha256:" + strings.Join(parts, ":")
}

// PEM returns the PEM encoding of a certificate's leaf, for transmitting
// over the wire during enrollment.
func PEM(cert tls.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
}

// FingerprintFromPEM parses a PEM-encoded certificate and returns its
// fingerprint, for the controller side verifying what an agent sent.
func FingerprintFromPEM(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("invalid certificate PEM")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return "", fmt.Errorf("parse certificate: %w", err)
	}
	return Fingerprint(block.Bytes), nil
}
