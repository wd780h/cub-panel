// Self-signed TLS for the agent API.
//
// The agent serves HTTPS by default. On first start it mints a 10-year
// self-signed ECDSA certificate; the panel does not chase a CA chain but
// pins the certificate's SHA-256 fingerprint per node (the same trust model
// Incus itself uses between cluster members). Every request is additionally
// HMAC-signed, so TLS adds confidentiality on top of existing authenticity.
package agent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"time"
)

// EnsureTLSCert returns the SHA-256 fingerprint of the certificate at
// certPath, generating a fresh self-signed pair first when none exists.
func EnsureTLSCert(certPath, keyPath string) (string, error) {
	if _, err := os.Stat(certPath); err == nil {
		return certFingerprint(certPath)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", err
	}
	host, _ := os.Hostname()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "cub-agent"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0), // ten years
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if host != "" {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return "", err
	}

	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

// certFingerprint reads an existing PEM certificate and hashes its DER form.
func certFingerprint(certPath string) (string, error) {
	raw, err := os.ReadFile(certPath)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("no certificate found in " + certPath)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", certPath, err)
	}
	if time.Now().After(cert.NotAfter) {
		return "", fmt.Errorf("certificate %s has expired; delete it to regenerate", certPath)
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:]), nil
}
