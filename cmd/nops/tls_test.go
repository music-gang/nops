package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writePair writes a self-signed certificate with the given serial and its key
// to cert and key.
func writePair(t *testing.T, cert, key string, serial int64) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "nops.test"},
		DNSNames:     []string{"nops.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// served is the serial of the certificate the server hands out.
func served(t *testing.T, c *certificate) int64 {
	t.Helper()
	pair, err := c.get(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	return pair.Leaf.SerialNumber.Int64()
}

// newTestCertificate loads a pair of serial 1 and returns the certificate, its
// two files and what it logs.
func newTestCertificate(t *testing.T) (*certificate, string, string, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "nops.crt"), filepath.Join(dir, "nops.key")
	writePair(t, cert, key, 1)
	logs := new(bytes.Buffer)
	c, err := newCertificate(cert, key, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return c, cert, key, logs
}

func TestCertificateReload(t *testing.T) {
	c, cert, key, logs := newTestCertificate(t)
	if got := served(t, c); got != 1 {
		t.Fatalf("serial = %d, want 1", got)
	}

	writePair(t, cert, key, 2)
	c.reload()
	if got := served(t, c); got != 2 {
		t.Errorf("serial after a reload = %d, want 2", got)
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("a good reload logged an ERROR: %s", logs)
	}
}

func TestCertificateReloadKeepsThePairOnFailure(t *testing.T) {
	tests := []struct {
		name   string
		damage func(cert, key string)
	}{
		{"not a certificate", func(cert, _ string) { _ = os.WriteFile(cert, []byte("junk"), 0o600) }},
		{"a file gone", func(_, key string) { _ = os.Remove(key) }},
		{"a key that is not the certificate's", func(cert, key string) {
			writePair(t, filepath.Join(filepath.Dir(cert), "other.crt"), key, 3) // a new key; the certificate stays
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, cert, key, logs := newTestCertificate(t)
			tt.damage(cert, key)
			c.reload()
			if got := served(t, c); got != 1 {
				t.Errorf("serial after a failed reload = %d, want the current 1", got)
			}
			if !strings.Contains(logs.String(), "level=ERROR") {
				t.Errorf("no ERROR logged: %s", logs)
			}
		})
	}
}

func TestNewCertificateFails(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(new(bytes.Buffer), nil))
	if _, err := newCertificate(filepath.Join(dir, "nops.crt"), filepath.Join(dir, "nops.key"), log); err == nil {
		t.Error("a missing pair started")
	}
}

func TestReloadOnSIGHUP(t *testing.T) {
	c, cert, key, _ := newTestCertificate(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := c.reloadOnSIGHUP(ctx)

	writePair(t, cert, key, 2)
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for served(t, c) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("SIGHUP did not reload the pair")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher did not stop with its context")
	}
}
