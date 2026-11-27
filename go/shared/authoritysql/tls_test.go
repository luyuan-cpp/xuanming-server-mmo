package authoritysql

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func writeTestCertPEM(t *testing.T, extraSuffix string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-authority-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, append(pemBytes, []byte(extraSuffix)...), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestValidateDSNTLSSafety(t *testing.T) {
	okCfg, _ := mysql.ParseDSN("user:pass@tcp(127.0.0.1:4000)/mmorpg_test")
	if err := ValidateDSNTLSSafety(okCfg); err != nil {
		t.Fatalf("plain DSN unexpectedly rejected: %v", err)
	}

	for _, dsn := range []string{
		"user:pass@tcp(127.0.0.1:4000)/mmorpg_test?tls=skip-verify",
		"user:pass@tcp(127.0.0.1:4000)/mmorpg_test?tls=preferred",
		"user:pass@tcp(127.0.0.1:4000)/mmorpg_test?allowFallbackToPlaintext=true",
	} {
		cfg, err := mysql.ParseDSN(dsn)
		if err != nil {
			t.Fatalf("ParseDSN(%q): %v", dsn, err)
		}
		if err := ValidateDSNTLSSafety(cfg); !errors.Is(err, ErrUnsafeTLSConfig) {
			t.Fatalf("DSN %q err = %v, want ErrUnsafeTLSConfig", dsn, err)
		}
	}

	lowTLS, _ := mysql.ParseDSN("user:pass@tcp(127.0.0.1:4000)/mmorpg_test")
	lowTLS.TLS = &tls.Config{MinVersion: tls.VersionTLS10}
	if err := ValidateDSNTLSSafety(lowTLS); !errors.Is(err, ErrUnsafeTLSConfig) {
		t.Fatalf("TLS 1.0 err = %v, want ErrUnsafeTLSConfig", err)
	}
}

func TestLoadStrictRootCAsAndOpenUnverifiedTLS(t *testing.T) {
	goodCA := writeTestCertPEM(t, "")
	if pool, err := LoadStrictRootCAs(goodCA); err != nil || pool == nil {
		t.Fatalf("LoadStrictRootCAs(good) = (%v, %v)", pool, err)
	}

	badCA := writeTestCertPEM(t, "\ntrailing-garbage\n")
	if _, err := LoadStrictRootCAs(badCA); err == nil {
		t.Fatal("trailing garbage in CA PEM must be rejected")
	}

	// OpenUnverified must reject tls=skip-verify automatically.
	if _, err := OpenUnverified(
		"user:pass@tcp(127.0.0.1:4000)/mmorpg_test?tls=skip-verify",
		"mmorpg_test", BackendTiDB, 2, Reader(),
	); !errors.Is(err, ErrUnsafeTLSConfig) {
		t.Fatalf("OpenUnverified(skip-verify) err = %v, want ErrUnsafeTLSConfig", err)
	}

	// WithStrictTLS rejects host mismatch.
	if _, err := OpenUnverified(
		"user:pass@tcp(127.0.0.1:4000)/mmorpg_test",
		"mmorpg_test", BackendTiDB, 2, Reader(),
		WithStrictTLS(goodCA, "tidb.internal"),
	); !errors.Is(err, ErrUnsafeTLSConfig) {
		t.Fatalf("OpenUnverified(host mismatch) err = %v, want ErrUnsafeTLSConfig", err)
	}

	// WithStrictTLS accepts matching host and valid bundle-only CA.
	db, err := OpenUnverified(
		"user:pass@tcp(tidb.internal:4000)/mmorpg_test",
		"mmorpg_test", BackendTiDB, 2, Reader(),
		WithStrictTLS(goodCA, "tidb.internal"),
	)
	if err != nil {
		t.Fatalf("OpenUnverified(valid strict TLS) failed: %v", err)
	}
	_ = db.Close()
}
