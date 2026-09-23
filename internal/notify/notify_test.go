package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/store"
)

// received is one request seen by the test server.
type received struct {
	path   string
	method string
	header http.Header
	body   []byte
}

// server records every request and answers with status.
type server struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []received
}

func newServer(t *testing.T, status int) *server {
	t.Helper()
	s := &server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.reqs = append(s.reqs, received{r.URL.Path, r.Method, r.Header.Clone(), b})
		s.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) requests() []received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]received(nil), s.reqs...)
}

func logger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

var (
	updated = time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	failed  = &store.Deployment{
		ID: "01J8ZX", JobID: "web", Namespace: "apps", CommitSHA: "0123456789abcdef0123",
		State: store.StateFailed, Error: "pre-hook web-migrate failed: exit 1", UpdatedAt: updated,
	}
	pending = &store.Deployment{
		ID: "01J8ZY", JobID: "api", Namespace: "default", CommitSHA: "fedcba9876543210",
		State: store.StatePendingApproval, UpdatedAt: updated,
	}
)

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, b)
	}
	return m
}

func TestWebhook(t *testing.T) {
	srv := newServer(t, http.StatusOK)
	log, buf := logger()
	New(Options{Webhook: Endpoint{URL: srv.URL + "/hook", Token: "tok"}, PublicURL: "https://nops.example.com/", Timeout: time.Second}, log).
		Notify(context.Background(), failed)

	reqs := srv.requests()
	if len(reqs) != 1 || buf.Len() > 0 {
		t.Fatalf("requests = %d, log = %s", len(reqs), buf)
	}
	r := reqs[0]
	if r.method != http.MethodPost || r.path != "/hook" || r.header.Get("Content-Type") != "application/json" || r.header.Get("Authorization") != "Bearer tok" {
		t.Errorf("request = %s %s, headers %v", r.method, r.path, r.header)
	}
	var got Event
	if err := json.Unmarshal(r.body, &got); err != nil {
		t.Fatal(err)
	}
	want := Event{
		DeploymentID: "01J8ZX", Job: "web", Namespace: "apps", State: "failed",
		Error: "pre-hook web-migrate failed: exit 1", Commit: "0123456789abcdef0123",
		URL: "https://nops.example.com/deployments/01J8ZX", Time: updated,
	}
	if got != want {
		t.Errorf("payload\n got %+v\nwant %+v", got, want)
	}
	for _, k := range []string{"deployment_id", "job", "namespace", "state", "error", "commit", "url", "time"} {
		if _, ok := decode(t, r.body)[k]; !ok {
			t.Errorf("payload has no %q key", k)
		}
	}
}

func TestWebhookWithoutTokenAndLink(t *testing.T) {
	srv := newServer(t, http.StatusNoContent)
	log, _ := logger()
	New(Options{Webhook: Endpoint{URL: srv.URL}, Timeout: time.Second}, log).Notify(context.Background(), pending)
	r := srv.requests()[0]
	if r.header.Get("Authorization") != "" {
		t.Error("Authorization sent without a token")
	}
	if m := decode(t, r.body); m["url"] != "" {
		t.Errorf("url = %v, want empty without a public URL", m["url"])
	}
}

func TestDiscord(t *testing.T) {
	srv := newServer(t, http.StatusNoContent)
	log, buf := logger()
	New(Options{Discord: Endpoint{URL: srv.URL + "/api/webhooks/1/secret"}, PublicURL: "https://nops.example.com", Timeout: time.Second}, log).
		Notify(context.Background(), failed)
	if buf.Len() > 0 {
		t.Fatal(buf)
	}
	r := srv.requests()[0]
	if r.path != "/api/webhooks/1/secret" || r.header.Get("Content-Type") != "application/json" {
		t.Errorf("request = %s, %v", r.path, r.header)
	}
	var body struct {
		Username string
		Embeds   []struct {
			Title, Description, URL string
			Color                   int
			Fields                  []struct{ Name, Value string }
		}
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		t.Fatal(err)
	}
	e := body.Embeds[0]
	if e.Title != "web failed" || e.Description != failed.Error || e.URL != "https://nops.example.com/deployments/01J8ZX" || e.Color != colorFailed {
		t.Errorf("embed = %+v", e)
	}
	if len(e.Fields) != 2 || e.Fields[0].Value != "apps" || e.Fields[1].Value != "0123456789ab" {
		t.Errorf("fields = %+v", e.Fields)
	}

	// Pending: amber, no description, and a long error is truncated.
	long := *failed
	long.Error = strings.Repeat("é", discordDescriptionMax+10)
	long.JobID = strings.Repeat("j", discordTitleMax+10)
	d, _ := discord(Event{Job: long.JobID, State: "failed", Error: long.Error})
	if err := json.Unmarshal(d.body, &body); err != nil {
		t.Fatal(err)
	}
	e = body.Embeds[0]
	if n := len([]rune(e.Description)); n != discordDescriptionMax {
		t.Errorf("description has %d runes, want %d", n, discordDescriptionMax)
	}
	if n := len([]rune(e.Title)); n != discordTitleMax {
		t.Errorf("title has %d runes, want %d", n, discordTitleMax)
	}
	p, _ := discord(Event{Job: "api", State: "pending_approval"})
	if err := json.Unmarshal(p.body, &body); err != nil {
		t.Fatal(err)
	}
	if e := body.Embeds[0]; e.Color != colorPending || e.Title != "api waiting for approval" || e.Fields[0].Value != "-" {
		t.Errorf("pending embed = %+v", e)
	}
}

func TestSlack(t *testing.T) {
	srv := newServer(t, http.StatusOK)
	log, _ := logger()
	d := *failed
	d.Error = "bad <input> & more\nsecond line"
	New(Options{Slack: Endpoint{URL: srv.URL}, PublicURL: "https://nops.example.com", Timeout: time.Second}, log).
		Notify(context.Background(), &d)
	m := decode(t, srv.requests()[0].body)
	want := "*web failed*\nNamespace: apps · Commit: `0123456789ab`\n>bad &lt;input&gt; &amp; more\n>second line\n<https://nops.example.com/deployments/01J8ZX|Open in nops>"
	if m["text"] != want {
		t.Errorf("text\n got %q\nwant %q", m["text"], want)
	}
}

func TestNtfy(t *testing.T) {
	srv := newServer(t, http.StatusOK)
	log, _ := logger()
	New(Options{Ntfy: Endpoint{URL: srv.URL + "/nops", Token: "tk"}, PublicURL: "https://nops.example.com", Timeout: time.Second}, log).
		Notify(context.Background(), failed)
	r := srv.requests()[0]
	h := r.header
	if r.path != "/nops" || h.Get("Title") != "web failed" || h.Get("Priority") != "4" || h.Get("Tags") != "rotating_light" ||
		h.Get("Click") != "https://nops.example.com/deployments/01J8ZX" || h.Get("Authorization") != "Bearer tk" ||
		!strings.HasPrefix(h.Get("Content-Type"), "text/plain") {
		t.Errorf("request %s, headers %v", r.path, h)
	}
	want := "Namespace: apps\nCommit: 0123456789ab\nError: pre-hook web-migrate failed: exit 1\nhttps://nops.example.com/deployments/01J8ZX"
	if string(r.body) != want {
		t.Errorf("body\n got %q\nwant %q", r.body, want)
	}

	req, _ := ntfy(Event{Job: "api", State: "pending_approval"}, "")
	if req.header.Get("Priority") != "3" || req.header.Get("Click") != "" || req.header.Get("Authorization") != "" {
		t.Errorf("pending headers %v", req.header)
	}
}

func TestGotify(t *testing.T) {
	srv := newServer(t, http.StatusOK)
	log, _ := logger()
	New(Options{Gotify: Endpoint{URL: srv.URL + "/", Token: "app"}, PublicURL: "https://nops.example.com", Timeout: time.Second}, log).
		Notify(context.Background(), failed)
	r := srv.requests()[0]
	if r.path != "/message" || r.header.Get("X-Gotify-Key") != "app" || r.header.Get("Content-Type") != "application/json" {
		t.Errorf("request %s, headers %v", r.path, r.header)
	}
	m := decode(t, r.body)
	if m["title"] != "web failed" || m["priority"] != float64(8) || !strings.Contains(m["message"].(string), "Error: pre-hook") {
		t.Errorf("body = %v", m)
	}
	click := m["extras"].(map[string]any)["client::notification"].(map[string]any)["click"].(map[string]any)["url"]
	if click != "https://nops.example.com/deployments/01J8ZX" {
		t.Errorf("click url = %v", click)
	}

	req, _ := gotify(Event{Job: "api", State: "pending_approval"}, "app")
	m = decode(t, req.body)
	if m["priority"] != float64(5) || m["extras"] != nil {
		t.Errorf("pending body = %v", m)
	}
}

func TestEveryAdapterReceives(t *testing.T) {
	ok := newServer(t, http.StatusOK)
	broken := newServer(t, http.StatusInternalServerError)
	log, buf := logger()
	New(Options{
		Webhook: Endpoint{URL: broken.URL + "/webhook"},
		Discord: Endpoint{URL: ok.URL + "/discord"},
		Slack:   Endpoint{URL: ok.URL + "/slack"},
		Ntfy:    Endpoint{URL: ok.URL + "/ntfy"},
		Gotify:  Endpoint{URL: ok.URL + "/gotify", Token: "t"},
		Timeout: time.Second,
	}, log).Notify(context.Background(), pending)

	var paths []string
	for _, r := range ok.requests() {
		paths = append(paths, r.path)
	}
	if got := strings.Join(paths, " "); got != "/discord /slack /ntfy /gotify/message" {
		t.Errorf("paths = %s", got)
	}
	if len(broken.requests()) != 1 {
		t.Errorf("webhook got %d requests", len(broken.requests()))
	}
	if strings.Count(buf.String(), "notification not delivered") != 1 || !strings.Contains(buf.String(), "adapter=webhook") {
		t.Errorf("log = %s", buf)
	}
}

func TestFailuresAreLoggedNotReturned(t *testing.T) {
	const secret = "/api/webhooks/1/do-not-log"

	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) }) // runs first: unblocks the handler so Close returns
	refused := newServer(t, http.StatusOK)
	refusedURL := refused.URL
	refused.Close()
	notFound := newServer(t, http.StatusNotFound)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		url  string
		ctx  context.Context
		want string
	}{
		{"non-2xx", notFound.URL, context.Background(), "answered 404 Not Found"},
		{"timeout", slow.URL, context.Background(), "Client.Timeout"},
		{"unreachable", refusedURL, context.Background(), "connection refused"},
		{"cancelled", notFound.URL, cancelled, "context canceled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log, buf := logger()
			start := time.Now()
			New(Options{Discord: Endpoint{URL: tt.url + secret}, Timeout: 50 * time.Millisecond}, log).Notify(tt.ctx, failed)
			if time.Since(start) > 5*time.Second {
				t.Error("Notify did not respect the timeout")
			}
			out := buf.String()
			if strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, tt.want) {
				t.Errorf("log = %s, want one WARN with %q", out, tt.want)
			}
			for _, k := range []string{"adapter=discord", "deployment_id=01J8ZX", "job=web", "namespace=apps", "state=failed"} {
				if !strings.Contains(out, k) {
					t.Errorf("log misses %s: %s", k, out)
				}
			}
			if strings.Contains(out, "do-not-log") {
				t.Errorf("log contains the URL: %s", out)
			}
		})
	}
}

func TestNoAdapter(t *testing.T) {
	log, buf := logger()
	New(Options{Timeout: time.Second}, log).Notify(context.Background(), failed)
	if buf.Len() > 0 {
		t.Errorf("log = %s", buf)
	}
}

func TestInvalidURLIsNotLogged(t *testing.T) {
	log, buf := logger()
	New(Options{Slack: Endpoint{URL: "http://bad host/secret-token"}, Timeout: time.Second}, log).Notify(context.Background(), failed)
	if !strings.Contains(buf.String(), "invalid URL") || strings.Contains(buf.String(), "secret-token") {
		t.Errorf("log = %s", buf)
	}
}
