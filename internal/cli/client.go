package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// requestTimeout bounds a request: Deploy now runs a detection cycle before it
// answers, which is the longest thing the API does.
const requestTimeout = time.Minute

// maxAnswer is the most of an answer the client reads: a diff is the largest.
const maxAnswer = 32 << 20

// settings is what every command needs besides its own arguments, from its
// flags first and then from the environment.
type settings struct {
	addr, tokenFile, namespace string
}

// client calls the API of docs/api.md with a bearer token.
type client struct {
	base    string // without trailing slash; may carry a sub path
	token   string // empty for the login, which has none yet
	session bool   // the token is the one nops login saved
	http    *http.Client
}

// statusError is an answer of Nops with a status of 300 or more.
type statusError struct {
	status  int
	message string
}

func (e *statusError) Error() string { return e.message }

// resolveAddr is the URL of Nops, from a flag or NOPS_ADDR, without a trailing slash.
func resolveAddr(flagAddr string, getenv func(string) string) (string, error) {
	addr := first(flagAddr, getenv("NOPS_ADDR"))
	if addr == "" {
		return "", usageErrorf("the URL of Nops is missing: set -addr or NOPS_ADDR")
	}
	u, err := url.Parse(addr)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", usageErrorf("the URL of Nops must be http:// or https:// with a host, got %q", addr)
	}
	return strings.TrimRight(addr, "/"), nil
}

// newClient resolves the URL and the token, or says which of them is missing.
// The token comes from a file (a flag, or NOPS_TOKEN_FILE), then NOPS_TOKEN,
// then the session nops login saved for the same URL. It is never part of an
// error message.
func newClient(s settings, getenv func(string) string) (*client, error) {
	addr, err := resolveAddr(s.addr, getenv)
	if err != nil {
		return nil, err
	}

	var token string
	fromSession := false
	if file := first(s.tokenFile, getenv("NOPS_TOKEN_FILE")); file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read the token file: %w", err)
		}
		token = strings.TrimSpace(string(b))
		if token == "" {
			return nil, fmt.Errorf("the token file %s is empty", file)
		}
	} else {
		token = strings.TrimSpace(getenv("NOPS_TOKEN"))
	}
	hint := ""
	if token == "" {
		saved, err := loadSession()
		switch {
		case err != nil:
			return nil, err
		case saved != nil && saved.Addr == addr && time.Now().After(saved.ExpiresAt):
			return nil, usageErrorf("your session at %s ended %s: run nops login", addr, stamp(saved.ExpiresAt))
		case saved != nil && saved.Addr == addr:
			token, fromSession = saved.Secret, true
		case saved != nil:
			hint = fmt.Sprintf(" (the saved session is for %s)", saved.Addr)
		}
	}
	if token == "" {
		return nil, usageErrorf("the API token is missing: set -token-file, NOPS_TOKEN_FILE or NOPS_TOKEN, or run nops login%s", hint)
	}
	return &client{base: addr, token: token, session: fromSession, http: &http.Client{Timeout: requestTimeout}}, nil
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// do sends a request and returns the body of a 2xx answer. A body, when given,
// is sent as JSON. Any other status is an error carrying the API's message.
func (c *client) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The URL carries no secret, but net/http's error is enough without it.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return nil, fmt.Errorf("%s %s: read the answer: %w", method, path, err)
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		msg := fmt.Sprintf("nops answered %d %s: is the URL the one of Nops?", resp.StatusCode, http.StatusText(resp.StatusCode))
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			msg = fmt.Sprintf("nops answered %d: %s", resp.StatusCode, e.Error)
		}
		if resp.StatusCode == http.StatusUnauthorized && c.session {
			msg += ": your session ended or was revoked: run nops login"
		}
		return nil, &statusError{status: resp.StatusCode, message: msg}
	}
	return b, nil
}

// get decodes a JSON answer into v and returns it raw too, for -json.
func (c *client) get(ctx context.Context, path string, v any) ([]byte, error) {
	b, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return nil, fmt.Errorf("GET %s: the answer is not what the API sends: is the URL the one of Nops?", path)
	}
	return b, nil
}

func jobPath(namespace, job string) string {
	return "/api/jobs/" + url.PathEscape(namespace) + "/" + url.PathEscape(job)
}

func deploymentPath(id string) string {
	return "/api/deployments/" + url.PathEscape(id)
}
