package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hashicorp/nomad/api"

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

const (
	publicURL     = "https://nops.example.com"
	commitSHA     = "0123456789abcdef0123"
	commitBase    = "https://git.example.com/ops/jobs/commit/"
	nomadUI       = "https://nomad.example.com"
	deploymentURL = publicURL + "/deployments/01J8ZX"
	commitLink    = commitBase + commitSHA
	nomadLink     = nomadUI + "/ui/jobs/web@apps"
	logoAddress   = publicURL + "/static/logo-192x192.png"
)

var (
	updated = time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	failed  = &store.Deployment{
		ID: "01J8ZX", JobID: "web", Namespace: "apps", CommitSHA: commitSHA,
		CommitSubject: "Bump web to 1.4.0", CommitAuthor: "Alice",
		Policy: store.PolicyApproval, DecidedBy: "alice",
		State: store.StateFailed, Error: "pre-hook web-migrate failed: exit 1", UpdatedAt: updated,
	}
	// pending is a deployment created before the commit subject and author
	// were recorded, with no plan diff.
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

// withState is a copy of d in another state.
func withState(d *store.Deployment, state store.State, mod func(*store.Deployment)) *store.Deployment {
	cp := *d
	cp.State = state
	if mod != nil {
		mod(&cp)
	}
	return &cp
}

// twoGroupsThreeTasks is a plan diff the notification sums up as "2 groups, 3
// tasks changed".
func twoGroupsThreeTasks(t *testing.T) string {
	t.Helper()
	edit := []*api.FieldDiff{{Type: "Edited", Name: "Count"}}
	task := func(name string) *api.TaskDiff { return &api.TaskDiff{Type: "Edited", Name: name, Fields: edit} }
	b, err := json.Marshal(&api.JobDiff{Type: "Edited", TaskGroups: []*api.TaskGroupDiff{
		{Type: "Edited", Name: "a", Fields: edit, Tasks: []*api.TaskDiff{task("x"), task("y")}},
		{Type: "Edited", Name: "b", Fields: edit, Tasks: []*api.TaskDiff{task("z")}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// allAdapters is an Options with every adapter pointing at its own receiver
// and every link configured.
func allAdapters(t *testing.T) (Options, map[string]*server) {
	t.Helper()
	srvs := map[string]*server{}
	o := Options{
		PublicURL:  publicURL + "/",
		CommitURL:  func(sha string) string { return commitBase + sha },
		NomadUIURL: nomadUI + "/",
		Timeout:    time.Second,
	}
	for name, dst := range map[string]*Endpoint{"webhook": &o.Webhook, "discord": &o.Discord, "slack": &o.Slack, "ntfy": &o.Ntfy, "gotify": &o.Gotify} {
		srvs[name] = newServer(t, http.StatusOK)
		dst.URL = srvs[name].URL
	}
	return o, srvs
}

// notifyAll sends d to every adapter and returns what each received.
func notifyAll(t *testing.T, o Options, srvs map[string]*server, d *store.Deployment, phase string) map[string]received {
	t.Helper()
	log, buf := logger()
	New(o, log).Notify(context.Background(), d, phase)
	if buf.Len() > 0 {
		t.Fatalf("log = %s", buf)
	}
	got := map[string]received{}
	for name, srv := range srvs {
		reqs := srv.requests()
		if len(reqs) != 1 {
			t.Fatalf("%s: %d requests, want 1", name, len(reqs))
		}
		got[name] = reqs[0]
	}
	return got
}

type discordBody struct {
	Username        string `json:"username"`
	AvatarURL       string `json:"avatar_url"`
	Content         string `json:"content"`
	AllowedMentions struct {
		Parse []string `json:"parse"`
	} `json:"allowed_mentions"`
	Embeds []struct {
		Title, Description, URL string
		Color                   int
		Fields                  []struct {
			Name, Value string
			Inline      bool
		}
	} `json:"embeds"`
}

func decodeDiscord(t *testing.T, b []byte) discordBody {
	t.Helper()
	var body discordBody
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Embeds) != 1 {
		t.Fatalf("%d embeds, want 1", len(body.Embeds))
	}
	return body
}

func TestWebhook(t *testing.T) {
	srv := newServer(t, http.StatusOK)
	log, buf := logger()
	New(Options{
		Webhook: Endpoint{URL: srv.URL + "/hook", Token: "tok"}, PublicURL: publicURL + "/", Timeout: time.Second,
		CommitURL: func(sha string) string { return commitBase + sha }, NomadUIURL: nomadUI,
	}, log).Notify(context.Background(), withState(failed, store.StateFailed, func(d *store.Deployment) { d.RetryOf, d.WindowLiftedBy = "01J8ZW", "bob" }), "pre")

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
		DeploymentID: "01J8ZX", Job: "web", Namespace: "apps", State: "failed", Phase: "pre",
		Policy: "approval", ApprovedBy: "alice", RetryOf: "01J8ZW", DeployedNowBy: "bob",
		Error: "pre-hook web-migrate failed: exit 1", Commit: commitSHA,
		CommitSubject: "Bump web to 1.4.0", CommitAuthor: "Alice", CommitURL: commitLink,
		URL: deploymentURL, NomadURL: nomadLink, Time: updated,
	}
	if got != want {
		t.Errorf("payload\n got %+v\nwant %+v", got, want)
	}
	// The keys receivers already read keep their names.
	for _, k := range []string{"deployment_id", "job", "namespace", "state", "error", "commit", "url", "time"} {
		if _, ok := decode(t, r.body)[k]; !ok {
			t.Errorf("payload has no %q key", k)
		}
	}
}

func TestWebhookWithoutTokenAndLinks(t *testing.T) {
	srv := newServer(t, http.StatusNoContent)
	log, _ := logger()
	New(Options{Webhook: Endpoint{URL: srv.URL}, Timeout: time.Second}, log).Notify(context.Background(), pending, "")
	r := srv.requests()[0]
	if r.header.Get("Authorization") != "" {
		t.Error("Authorization sent without a token")
	}
	m := decode(t, r.body)
	if m["url"] != "" {
		t.Errorf("url = %v, want empty without a public URL", m["url"])
	}
	// What a notification has no value for is left out, not sent empty.
	for _, k := range []string{"waiting", "phase", "policy", "approved_by", "commit_subject", "commit_author", "commit_url", "nomad_url", "retry_of", "deployed_now_by", "changes"} {
		if v, ok := m[k]; ok {
			t.Errorf("payload has %q = %v, want the key left out", k, v)
		}
	}
}

// The preview is what a system notification shows before anyone opens it: the
// title and the first line of the body. Every adapter says the same.
func TestPreviewIsTheSameInEveryAdapter(t *testing.T) {
	canary := &store.Deployment{
		ID: "01J8ZX", JobID: "web", Namespace: "apps", CommitSHA: commitSHA,
		State: store.StateApplying, PromotionWaitSince: updated, UpdatedAt: updated,
	}
	for name, tt := range map[string]struct {
		d                     *store.Deployment
		phase, headline, lead string
		emoji, tag            string
	}{
		"pending approval": {
			d:     withState(failed, store.StatePendingApproval, func(d *store.Deployment) { d.Error, d.DecidedBy = "", "" }),
			emoji: "⏳", tag: "hourglass_flowing_sand", headline: "web needs approval", lead: "Bump web to 1.4.0 · Alice",
		},
		"canary promotion": {
			d:     canary,
			emoji: "🐤", tag: "baby_chick", headline: "web canaries need promotion", lead: "promote them in Nomad",
		},
		"failed in the pre-hook": {
			d: failed, phase: "pre",
			emoji: "❌", tag: "x", headline: "web failed in the pre-hook", lead: "pre-hook web-migrate failed: exit 1",
		},
		"failed in detection": {
			d: failed, phase: "detection",
			emoji: "❌", tag: "x", headline: "web failed in detection", lead: "pre-hook web-migrate failed: exit 1",
		},
		"failed in the apply": {
			d: failed, phase: "apply",
			emoji: "❌", tag: "x", headline: "web failed in the apply", lead: "pre-hook web-migrate failed: exit 1",
		},
		"failed in the post-hook": {
			d: failed, phase: "post",
			emoji: "❌", tag: "x", headline: "web failed in the post-hook", lead: "pre-hook web-migrate failed: exit 1",
		},
		"failed, many lines": {
			d: withState(failed, store.StateFailed, func(d *store.Deployment) { d.Error = "apply did not become healthy: unhealthy\nsecond line" }), phase: "apply",
			emoji: "❌", tag: "x", headline: "web failed in the apply", lead: "apply did not become healthy: unhealthy",
		},
		"completed": {
			d:     withState(failed, store.StateCompleted, func(d *store.Deployment) { d.Error = "" }),
			emoji: "✅", tag: "white_check_mark", headline: "web completed", lead: "Bump web to 1.4.0 · Alice",
		},
		"without a commit subject": {
			d:     withState(failed, store.StateCompleted, func(d *store.Deployment) { d.Error, d.CommitSubject, d.CommitAuthor = "", "", "" }),
			emoji: "✅", tag: "white_check_mark", headline: "web completed", lead: shortCommit(commitSHA),
		},
	} {
		t.Run(name, func(t *testing.T) {
			title := tt.emoji + " " + tt.headline
			o, srvs := allAdapters(t)
			got := notifyAll(t, o, srvs, tt.d, tt.phase)

			d := decodeDiscord(t, got["discord"].body)
			if want := title + "\n" + tt.lead; d.Content != want || d.Embeds[0].Title != title {
				t.Errorf("discord content %q, embed title %q; want %q and %q", d.Content, d.Embeds[0].Title, want, title)
			}

			s := decode(t, got["slack"].body)
			if want := "*" + title + "*\n" + tt.lead + "\n"; !strings.HasPrefix(s["text"].(string), want) {
				t.Errorf("slack text = %q, want it to start with %q", s["text"], want)
			}
			header := s["blocks"].([]any)[0].(map[string]any)["text"].(map[string]any)["text"]
			if header != title {
				t.Errorf("slack header = %v, want %q", header, title)
			}

			n := got["ntfy"]
			if n.header.Get("Title") != tt.headline || n.header.Get("Tags") != tt.tag || strings.SplitN(string(n.body), "\n", 2)[0] != tt.lead {
				t.Errorf("ntfy title %q, tags %q, body %q; want %q, %q and a first line %q", n.header.Get("Title"), n.header.Get("Tags"), n.body, tt.headline, tt.tag, tt.lead)
			}

			g := decode(t, got["gotify"].body)
			if g["title"] != title || strings.SplitN(g["message"].(string), "\n", 2)[0] != tt.lead {
				t.Errorf("gotify title %v, message %q; want %q and a first line %q", g["title"], g["message"], title, tt.lead)
			}
		})
	}
}

func TestDiscord(t *testing.T) {
	o, srvs := allAdapters(t)
	d := withState(failed, store.StateFailed, func(d *store.Deployment) { d.RetryOf, d.WindowLiftedBy = "01J8ZW", "bob" })
	r := notifyAll(t, o, srvs, d, "pre")["discord"]
	if r.header.Get("Content-Type") != "application/json" {
		t.Errorf("headers %v", r.header)
	}
	body := decodeDiscord(t, r.body)
	if body.Username != "Nops" || body.AvatarURL != logoAddress {
		t.Errorf("sender = %q with avatar %q, want Nops with the dashboard's logo %q", body.Username, body.AvatarURL, logoAddress)
	}
	if len(body.AllowedMentions.Parse) != 0 {
		t.Errorf("allowed mentions = %v, want none: a subject or an error can hold @everyone", body.AllowedMentions.Parse)
	}
	e := body.Embeds[0]
	if e.Description != failed.Error || e.URL != deploymentURL || e.Color != colorFailed {
		t.Errorf("embed = %+v", e)
	}
	type field struct {
		Name, Value string
		Inline      bool
	}
	want := []field{
		{"Commit", "[`0123456789ab`](" + commitLink + ") Bump web to 1.4.0 · Alice", false},
		{"Namespace", "apps", true},
		{"Policy", "approval", true},
		{"Approved by", "alice", true},
		{"Retry of", "01J8ZW", true},
		{"Deployed now by", "bob", true},
		{"Links", "[Open in Nops](" + deploymentURL + ") · [Commit](" + commitLink + ") · [Open in Nomad](" + nomadLink + ")", false},
	}
	if len(e.Fields) != len(want) {
		t.Fatalf("fields = %+v, want %+v", e.Fields, want)
	}
	for i, f := range e.Fields {
		if f != want[i] {
			t.Errorf("field %d = %+v, want %+v", i, f, want[i])
		}
	}

	// Waiting for approval: amber, with a sum-up of the changes and no error.
	p := withState(failed, store.StatePendingApproval, func(d *store.Deployment) { d.Error, d.DecidedBy, d.PlanDiff = "", "", twoGroupsThreeTasks(t) })
	o, srvs = allAdapters(t)
	e = decodeDiscord(t, notifyAll(t, o, srvs, p, "")["discord"].body).Embeds[0]
	if e.Color != colorPending || e.Description != "" {
		t.Errorf("pending embed = %+v", e)
	}
	var changes string
	for _, f := range e.Fields {
		if f.Name == "Changes" {
			changes = f.Value
		}
	}
	if changes != "2 groups, 3 tasks changed" {
		t.Errorf("changes = %q in %+v", changes, e.Fields)
	}

	o, srvs = allAdapters(t)
	if e := decodeDiscord(t, notifyAll(t, o, srvs, withState(failed, store.StateCompleted, nil), "")["discord"].body).Embeds[0]; e.Color != colorCompleted {
		t.Errorf("completed color = %x, want %x", e.Color, colorCompleted)
	}

	// What Markdown would change in a commit subject is escaped.
	o, srvs = allAdapters(t)
	d = withState(failed, store.StateFailed, func(d *store.Deployment) { d.CommitSubject = "fix_snake_case [x] *now*" })
	e = decodeDiscord(t, notifyAll(t, o, srvs, d, "pre")["discord"].body).Embeds[0]
	if got := e.Fields[0].Value; !strings.HasSuffix(got, `fix\_snake\_case \[x\] \*now\* · Alice`) {
		t.Errorf("commit field = %q", got)
	}
}

func TestDiscordLimits(t *testing.T) {
	long := Event{Job: strings.Repeat("j", discordTitleMax+10), State: "failed", Error: strings.Repeat("é", discordDescriptionMax+10), Namespace: strings.Repeat("n", discordFieldMax+10)}
	r, _ := discord(long)
	body := decodeDiscord(t, r.body)
	e := body.Embeds[0]
	if n := len([]rune(e.Description)); n != discordDescriptionMax {
		t.Errorf("description has %d runes, want %d", n, discordDescriptionMax)
	}
	if n := len([]rune(e.Title)); n != discordTitleMax {
		t.Errorf("title has %d runes, want %d", n, discordTitleMax)
	}
	if n := len([]rune(body.Content)); n > discordContentMax {
		t.Errorf("content has %d runes, want at most %d", n, discordContentMax)
	}
	for _, f := range e.Fields {
		if n := len([]rune(f.Value)); n > discordFieldMax {
			t.Errorf("field %s has %d runes, want at most %d", f.Name, n, discordFieldMax)
		}
	}
}

func TestSlack(t *testing.T) {
	o, srvs := allAdapters(t)
	d := withState(failed, store.StateFailed, func(d *store.Deployment) { d.Error = "bad <input> & more\nsecond line" })
	r := notifyAll(t, o, srvs, d, "pre")["slack"]
	m := decode(t, r.body)

	// The whole message, for the receivers that ignore blocks.
	want := "*❌ web failed in the pre-hook*\n" +
		"bad &lt;input&gt; &amp; more\n" +
		"Commit: <" + commitLink + "|`0123456789ab`> Bump web to 1.4.0 · Alice\n" +
		"Namespace: apps · Policy: approval · Approved by: alice\n" +
		">bad &lt;input&gt; &amp; more\n>second line\n" +
		"<" + deploymentURL + "|Open in Nops> · <" + commitLink + "|Commit> · <" + nomadLink + "|Open in Nomad>"
	if m["text"] != want {
		t.Errorf("text\n got %q\nwant %q", m["text"], want)
	}

	blocks := m["blocks"].([]any)
	var types []string
	for _, b := range blocks {
		types = append(types, b.(map[string]any)["type"].(string))
	}
	if got := strings.Join(types, " "); got != "header section context section actions" {
		t.Fatalf("blocks = %s", got)
	}
	text := func(i int) string { return blocks[i].(map[string]any)["text"].(map[string]any)["text"].(string) }
	if got, want := text(1), "<"+commitLink+"|`0123456789ab`> Bump web to 1.4.0 · Alice"; got != want {
		t.Errorf("commit section = %q, want %q", got, want)
	}
	if got, want := text(3), ">bad &lt;input&gt; &amp; more\n>second line"; got != want {
		t.Errorf("error section = %q, want %q", got, want)
	}
	ctx := blocks[2].(map[string]any)["elements"].([]any)[0].(map[string]any)["text"]
	if ctx != "Namespace: apps · Policy: approval · Approved by: alice" {
		t.Errorf("context = %v", ctx)
	}
	type button struct{ label, url string }
	var buttons []button
	for _, b := range blocks[4].(map[string]any)["elements"].([]any) {
		b := b.(map[string]any)
		buttons = append(buttons, button{b["text"].(map[string]any)["text"].(string), b["url"].(string)})
	}
	wantButtons := []button{{"Open in Nops", deploymentURL}, {"Commit", commitLink}, {"Open in Nomad", nomadLink}}
	if len(buttons) != len(wantButtons) {
		t.Fatalf("buttons = %v, want %v", buttons, wantButtons)
	}
	for i := range buttons {
		if buttons[i] != wantButtons[i] {
			t.Errorf("button %d = %v, want %v", i, buttons[i], wantButtons[i])
		}
	}
}

func TestSlackLimits(t *testing.T) {
	r, _ := slack(Event{Job: strings.Repeat("j", slackHeaderMax+10), State: "failed", Error: strings.Repeat("é", slackSectionMax+10)})
	for _, b := range decode(t, r.body)["blocks"].([]any) {
		b := b.(map[string]any)
		text, ok := b["text"].(map[string]any)
		if !ok {
			continue
		}
		max := slackSectionMax
		if b["type"] == "header" {
			max = slackHeaderMax
		}
		if n := len([]rune(text["text"].(string))); n > max {
			t.Errorf("%s block has %d runes, want at most %d", b["type"], n, max)
		}
	}
}

func TestNtfy(t *testing.T) {
	o, srvs := allAdapters(t)
	o.Ntfy.Token = "tk"
	d := withState(failed, store.StateFailed, func(d *store.Deployment) { d.RetryOf, d.WindowLiftedBy = "01J8ZW", "bob" })
	r := notifyAll(t, o, srvs, d, "pre")["ntfy"]
	h := r.header
	if h.Get("Title") != "web failed in the pre-hook" || h.Get("Priority") != "4" || h.Get("Tags") != "x" ||
		h.Get("Click") != deploymentURL || h.Get("Icon") != logoAddress || h.Get("Authorization") != "Bearer tk" ||
		!strings.HasPrefix(h.Get("Content-Type"), "text/plain") {
		t.Errorf("headers %v", h)
	}
	wantActions := "view, Open in Nops, " + deploymentURL + "; view, Commit, " + commitLink + "; view, Open in Nomad, " + nomadLink
	if got := h.Get("Actions"); got != wantActions {
		t.Errorf("Actions = %q, want %q", got, wantActions)
	}
	// The links are the buttons: the text has none.
	want := "pre-hook web-migrate failed: exit 1\n" +
		"Commit: 0123456789ab Bump web to 1.4.0 · Alice\n" +
		"Namespace: apps · Policy: approval · Approved by: alice · Retry of: 01J8ZW · Deployed now by: bob"
	if string(r.body) != want {
		t.Errorf("body\n got %q\nwant %q", r.body, want)
	}

	// Priority by state: completed needs no action, so it must not compete
	// with a failure or an approval for attention.
	for state, want := range map[store.State]string{store.StatePendingApproval: "3", store.StateCompleted: "2"} {
		o, srvs := allAdapters(t)
		if got := notifyAll(t, o, srvs, withState(failed, state, nil), "")["ntfy"].header.Get("Priority"); got != want {
			t.Errorf("%s: priority %q, want %q", state, got, want)
		}
	}

	// A long error is cut on a character, to what ntfy keeps as text.
	req, _ := ntfy(Event{Job: "web", State: "failed", Error: strings.Repeat("é", ntfyMessageMax)}, "")
	if len(req.body) > ntfyMessageMax || !utf8.Valid(req.body) {
		t.Errorf("body has %d bytes, valid UTF-8 %v; want at most %d", len(req.body), utf8.Valid(req.body), ntfyMessageMax)
	}
}

func TestGotify(t *testing.T) {
	o, srvs := allAdapters(t)
	o.Gotify.URL += "/"
	o.Gotify.Token = "app"
	p := withState(failed, store.StatePendingApproval, func(d *store.Deployment) { d.Error, d.DecidedBy, d.PlanDiff = "", "", twoGroupsThreeTasks(t) })
	r := notifyAll(t, o, srvs, p, "")["gotify"]
	if r.path != "/message" || r.header.Get("X-Gotify-Key") != "app" || r.header.Get("Content-Type") != "application/json" {
		t.Errorf("request %s, headers %v", r.path, r.header)
	}
	m := decode(t, r.body)
	want := "Bump web to 1.4.0 · Alice\n" +
		"Commit: 0123456789ab\n" +
		"Namespace: apps · Policy: approval\n" +
		"2 groups, 3 tasks changed\n" +
		"Open in Nops: " + deploymentURL + "\n" +
		"Commit: " + commitLink + "\n" +
		"Open in Nomad: " + nomadLink
	if m["title"] != "⏳ web needs approval" || m["priority"] != float64(5) || m["message"] != want {
		t.Errorf("body\n got %v\nwant message %q", m, want)
	}
	click := m["extras"].(map[string]any)["client::notification"].(map[string]any)["click"].(map[string]any)["url"]
	if click != deploymentURL {
		t.Errorf("click url = %v", click)
	}

	for state, want := range map[store.State]float64{store.StateFailed: 8, store.StateCompleted: 2} {
		o, srvs := allAdapters(t)
		if got := decode(t, notifyAll(t, o, srvs, withState(failed, state, nil), "")["gotify"].body)["priority"]; got != want {
			t.Errorf("%s: priority %v, want %v", state, got, want)
		}
	}
}

// A link whose URL is not configured is left out of every adapter, and so is
// the logo.
func TestWithoutLinksOrLogo(t *testing.T) {
	o, srvs := allAdapters(t)
	o.PublicURL, o.CommitURL, o.NomadUIURL = "", nil, ""
	got := notifyAll(t, o, srvs, failed, "pre")

	d := decodeDiscord(t, got["discord"].body)
	if d.AvatarURL != "" || d.Embeds[0].URL != "" {
		t.Errorf("discord avatar %q, embed URL %q, want none", d.AvatarURL, d.Embeds[0].URL)
	}
	for _, f := range d.Embeds[0].Fields {
		if f.Name == "Links" || strings.Contains(f.Value, "](") {
			t.Errorf("discord field %+v holds a link", f)
		}
	}

	s := decode(t, got["slack"].body)
	for _, b := range s["blocks"].([]any) {
		if b.(map[string]any)["type"] == "actions" {
			t.Error("slack has link buttons")
		}
	}
	if strings.Contains(s["text"].(string), "<http") {
		t.Errorf("slack text holds a link: %q", s["text"])
	}

	h := got["ntfy"].header
	for _, k := range []string{"Click", "Icon", "Actions"} {
		if h.Get(k) != "" {
			t.Errorf("ntfy %s = %q, want none", k, h.Get(k))
		}
	}

	g := decode(t, got["gotify"].body)
	if strings.Contains(g["message"].(string), "http") || g["extras"] != nil {
		t.Errorf("gotify = %v, want no link", g)
	}
}

// The sum-up of the changes belongs to a deployment waiting for approval, and a
// plan diff that does not decode never keeps the notification from going out.
func TestChanges(t *testing.T) {
	o, srvs := allAdapters(t)
	withDiff := func(d *store.Deployment) { d.PlanDiff = twoGroupsThreeTasks(t) }
	got := notifyAll(t, o, srvs, withState(failed, store.StateFailed, withDiff), "pre")
	if m := decode(t, got["webhook"].body); m["changes"] != nil {
		t.Errorf("failed payload has changes = %v", m["changes"])
	}

	o, srvs = allAdapters(t)
	got = notifyAll(t, o, srvs, withState(failed, store.StatePendingApproval, withDiff), "")
	if m := decode(t, got["webhook"].body); m["changes"] != "2 groups, 3 tasks changed" {
		t.Errorf("pending payload changes = %v", m["changes"])
	}

	srv := newServer(t, http.StatusOK)
	log, buf := logger()
	corrupt := withState(failed, store.StatePendingApproval, func(d *store.Deployment) { d.PlanDiff = "{" })
	New(Options{Webhook: Endpoint{URL: srv.URL}, Timeout: time.Second}, log).Notify(context.Background(), corrupt, "")
	reqs := srv.requests()
	if len(reqs) != 1 || decode(t, reqs[0].body)["changes"] != nil {
		t.Errorf("a corrupt plan diff: %d requests, body %s", len(reqs), reqs[0].body)
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "deployment_id=01J8ZX") {
		t.Errorf("log = %s, want a WARN naming the deployment", buf)
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
	}, log).Notify(context.Background(), pending, "")

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

// TestFailuresCountsEachAdapter checks the count /metrics exposes: every
// configured adapter is there, one that never failed with 0, and each failed
// delivery adds one to its own adapter only.
func TestFailuresCountsEachAdapter(t *testing.T) {
	ok := newServer(t, http.StatusOK)
	broken := newServer(t, http.StatusInternalServerError)
	log, _ := logger()
	n := New(Options{
		Webhook: Endpoint{URL: broken.URL + "/webhook"},
		Ntfy:    Endpoint{URL: ok.URL + "/ntfy"},
		Timeout: time.Second,
	}, log)
	want := map[string]uint64{"webhook": 0, "ntfy": 0}
	if got := n.Failures(); !maps.Equal(got, want) {
		t.Fatalf("Failures before any notification = %v, want %v", got, want)
	}
	n.Notify(context.Background(), failed, "")
	n.Notify(context.Background(), pending, "")
	want = map[string]uint64{"webhook": 2, "ntfy": 0}
	if got := n.Failures(); !maps.Equal(got, want) {
		t.Errorf("Failures = %v, want %v", got, want)
	}
	if got := New(Options{Timeout: time.Second}, log).Failures(); len(got) != 0 {
		t.Errorf("Failures without an adapter = %v, want none", got)
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
			New(Options{Discord: Endpoint{URL: tt.url + secret}, Timeout: 50 * time.Millisecond}, log).Notify(tt.ctx, failed, "")
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
	New(Options{Timeout: time.Second}, log).Notify(context.Background(), failed, "")
	if buf.Len() > 0 {
		t.Errorf("log = %s", buf)
	}
}

func TestInvalidURLIsNotLogged(t *testing.T) {
	log, buf := logger()
	New(Options{Slack: Endpoint{URL: "http://bad host/secret-token"}, Timeout: time.Second}, log).Notify(context.Background(), failed, "")
	if !strings.Contains(buf.String(), "invalid URL") || strings.Contains(buf.String(), "secret-token") {
		t.Errorf("log = %s", buf)
	}
}

// A deployment applying whose Nomad deployment waits for a person to promote
// its canaries is told to them by name, in every adapter, and once promoted (or
// with no wait at all) an applying deployment carries no such marker.
func TestWaitingForCanaryPromotion(t *testing.T) {
	waiting := &store.Deployment{
		ID: "01J8ZW", JobID: "web", Namespace: "apps", CommitSHA: commitSHA,
		State: store.StateApplying, PromotionWaitSince: updated, UpdatedAt: updated,
	}
	o, srvs := allAdapters(t)
	got := notifyAll(t, o, srvs, waiting, "")
	if m := decode(t, got["webhook"].body); m["state"] != "applying" || m["waiting"] != WaitingCanaryPromotion {
		t.Errorf("webhook payload = %v, want state applying and waiting %q", m, WaitingCanaryPromotion)
	}
	if got := decodeDiscord(t, got["discord"].body).Embeds[0].Title; got != "🐤 web canaries need promotion" {
		t.Errorf("discord title = %q", got)
	}

	log, _ := logger()
	for name, d := range map[string]*store.Deployment{
		"promoted":     {ID: "a", JobID: "web", State: store.StateApplying, PromotionWaitSince: updated, PromotedAt: updated},
		"never waited": {ID: "b", JobID: "web", State: store.StateApplying},
		"failed after": {ID: "c", JobID: "web", State: store.StateFailed, PromotionWaitSince: updated},
	} {
		srv := newServer(t, http.StatusOK)
		New(Options{Webhook: Endpoint{URL: srv.URL}, Timeout: time.Second}, log).Notify(context.Background(), d, "")
		if m := decode(t, srv.requests()[0].body); m["waiting"] != nil {
			t.Errorf("%s: payload = %v, want no waiting key", name, m)
		}
	}
}
