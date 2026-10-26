package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

// certificate is the pair the dashboard serves over TLS. It is read again on
// SIGHUP, so a renewed pair needs no restart.
type certificate struct {
	certFile, keyFile string
	pair              atomic.Pointer[tls.Certificate]
	log               *slog.Logger
}

// newCertificate loads the pair: one that can't be read, or whose key does not
// match the certificate, is an error that stops the start.
func newCertificate(certFile, keyFile string, log *slog.Logger) (*certificate, error) {
	c := &certificate{certFile: certFile, keyFile: keyFile, log: log}
	if err := c.load(); err != nil {
		return nil, err
	}
	return c, nil
}

// load reads both files and, if they are a valid pair, serves it from now on.
func (c *certificate) load() error {
	pair, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		return fmt.Errorf("load the TLS certificate and key: %w", err)
	}
	c.pair.Store(&pair)
	c.log.Info("loaded the TLS certificate", "cert_file", c.certFile, "not_after", pair.Leaf.NotAfter.UTC().Format(time.RFC3339))
	return nil
}

// reload is load for a running server: on failure the current pair stays and
// the error is logged.
func (c *certificate) reload() {
	if err := c.load(); err != nil {
		c.log.Error("the TLS pair was not reloaded, the current one stays", "cert_file", c.certFile, "key_file", c.keyFile, "error", err)
	}
}

// get is the GetCertificate of the server's tls.Config.
func (c *certificate) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return c.pair.Load(), nil
}

// reloadOnSIGHUP reloads the pair at each SIGHUP, until ctx is done. It is
// listening when it returns; the channel it returns closes when it stops.
func (c *certificate) reloadOnSIGHUP(ctx context.Context) <-chan struct{} {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer signal.Stop(signals)
		for {
			select {
			case <-ctx.Done():
				return
			case <-signals:
				c.log.Info("SIGHUP: reloading the TLS certificate")
				c.reload()
			}
		}
	}()
	return done
}
