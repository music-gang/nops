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
	base  string // without trailing slash; may carry a sub path
	token string
	http  *http.Client
}

// newClient resolves the URL and the token, or says which of them is missing.
// The token comes from a file (a flag, or NOPS_TOKEN_FILE) before NOPS_TOKEN,
// and it is never part of an error message.
func newClient(s settings, getenv func(string) string) (*client, error) {
	addr := first(s.addr, getenv("NOPS_ADDR"))
	if addr == "" {
		return nil, usageErrorf("the URL of Nops is missing: set -addr or NOPS_ADDR")
	}
	u, err := url.Parse(addr)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, usageErrorf("the URL of Nops must be http:// or https:// with a host, got %q", addr)
	}

	var token string
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
	if token == "" {
		return nil, usageErrorf("the API token is missing: set -token-file, NOPS_TOKEN_FILE or NOPS_TOKEN")
	}
	return &client{base: strings.TrimRight(addr, "/"), token: token, http: &http.Client{Timeout: requestTimeout}}, nil
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
	req.Header.Set("Authorization", "Bearer "+c.token)
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
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("nops answered %d: %s", resp.StatusCode, e.Error)
		}
		return nil, fmt.Errorf("nops answered %d %s: is the URL the one of Nops?", resp.StatusCode, http.StatusText(resp.StatusCode))
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
