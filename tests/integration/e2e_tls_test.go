//go:build integration

package integration

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// writeSelfSigned writes a certificate for 127.0.0.1 with the given serial and
// its key, and returns the certificate in PEM.
func writeSelfSigned(t *testing.T, certFile, keyFile string, serial int64) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "nops.test"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPEM
}

// TestE2EServesTLS runs nops with a tls block: it answers over HTTPS with
// Strict-Transport-Security, SIGHUP makes it serve a renewed pair, and it
// still stops cleanly on SIGTERM.
func TestE2EServesTLS(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "nops.crt"), filepath.Join(dir, "nops.key")
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(writeSelfSigned(t, certFile, keyFile, 1))

	config := filepath.Join(dir, "nops.hcl")
	if err := os.WriteFile(config, []byte(`
listen {
  tls {
    cert_file = "`+certFile+`"
    key_file  = "`+keyFile+`"
  }
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	proc := startNopsTLS(t, newScratchRepo(t).url, roots, "NOPS_CONFIG="+config)

	// served is the serial of the certificate nops presents now.
	served := func() int64 {
		t.Helper()
		resp, err := proc.client.Get(proc.baseURL + "/healthz")
		if err != nil {
			t.Fatalf("GET /healthz: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /healthz: status %d", resp.StatusCode)
		}
		if got := resp.Header.Get("Strict-Transport-Security"); got != "max-age=31536000" {
			t.Errorf("Strict-Transport-Security = %q, want max-age=31536000", got)
		}
		return resp.TLS.PeerCertificates[0].SerialNumber.Int64()
	}
	if got := served(); got != 1 {
		t.Fatalf("certificate serial = %d, want 1", got)
	}

	// A renewed pair is served after SIGHUP, with no restart.
	roots.AppendCertsFromPEM(writeSelfSigned(t, certFile, keyFile, 2))
	if err := proc.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := served(); got == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("nops still serves the first certificate after SIGHUP")
		}
		time.Sleep(100 * time.Millisecond)
	}

	proc.stop(t)
}
