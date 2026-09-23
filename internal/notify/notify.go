// Package notify tells people that a deployment needs them: it sends a
// notification to every configured adapter (generic webhook, Discord, Slack,
// ntfy, Gotify). A failed delivery is logged at WARN and never reaches the
// caller, so it cannot block the state machine.
//
// The adapters and their payloads are documented in
// docs/error-handling.md#notifications: keep the two aligned.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/music-gang/nops/internal/store"
)

// Options configures the adapters. An adapter is on when its URL is set.
type Options struct {
	Webhook   Endpoint // generic JSON POST; Token is sent as Authorization: Bearer
	Discord   Endpoint // webhook URL; the token is part of it
	Slack     Endpoint // incoming webhook URL; the token is part of it
	Ntfy      Endpoint // topic URL; Token is sent as Authorization: Bearer
	Gotify    Endpoint // server URL (messages go to <URL>/message); Token is the app token
	PublicURL string   // external URL of the dashboard; empty means no links
	Timeout   time.Duration
}

// Endpoint is where an adapter sends, and with which token.
type Endpoint struct {
	URL   string
	Token string
}

// Event is what a notification says. It is also the body of the generic
// webhook.
type Event struct {
	DeploymentID string    `json:"deployment_id"`
	Job          string    `json:"job"`
	Namespace    string    `json:"namespace"`
	State        string    `json:"state"`
	Error        string    `json:"error"`
	Commit       string    `json:"commit"`
	URL          string    `json:"url"` // link to the deployment; empty without a public URL
	Time         time.Time `json:"time"`
}

// request is what an adapter sends.
type request struct {
	body        []byte
	contentType string
	header      http.Header
}

type sender struct {
	name   string
	url    string
	format func(Event) (request, error)
}

// Notifier sends notifications. Build it with New.
type Notifier struct {
	senders   []sender
	publicURL string
	client    *http.Client
	log       *slog.Logger
}

// New builds a Notifier with one sender per adapter whose URL is set.
func New(o Options, log *slog.Logger) *Notifier {
	n := &Notifier{
		publicURL: strings.TrimRight(o.PublicURL, "/"),
		client:    &http.Client{Timeout: o.Timeout},
		log:       log,
	}
	add := func(name, url string, format func(Event) (request, error)) {
		if url != "" {
			n.senders = append(n.senders, sender{name: name, url: url, format: format})
		}
	}
	add("webhook", o.Webhook.URL, func(e Event) (request, error) { return webhook(e, o.Webhook.Token) })
	add("discord", o.Discord.URL, discord)
	add("slack", o.Slack.URL, slack)
	add("ntfy", o.Ntfy.URL, func(e Event) (request, error) { return ntfy(e, o.Ntfy.Token) })
	if o.Gotify.URL != "" {
		add("gotify", strings.TrimRight(o.Gotify.URL, "/")+"/message", func(e Event) (request, error) { return gotify(e, o.Gotify.Token) })
	}
	return n
}

// Notify sends d to every configured adapter, one after the other, each
// bounded by the timeout, without retries. It never returns an error: a
// failed delivery is logged at WARN and the other adapters still receive it.
// Call it after the transition to the notified state has been saved.
func (n *Notifier) Notify(ctx context.Context, d *store.Deployment) {
	if len(n.senders) == 0 {
		return
	}
	e := Event{
		DeploymentID: d.ID,
		Job:          d.JobID,
		Namespace:    d.Namespace,
		State:        string(d.State),
		Error:        d.Error,
		Commit:       d.CommitSHA,
		Time:         d.UpdatedAt.UTC(),
	}
	if n.publicURL != "" {
		e.URL = n.publicURL + "/deployments/" + url.PathEscape(d.ID)
	}
	for _, s := range n.senders {
		if err := n.send(ctx, s, e); err != nil {
			n.log.WarnContext(ctx, "notification not delivered",
				"adapter", s.name, "deployment_id", e.DeploymentID, "job", e.Job,
				"namespace", e.Namespace, "state", e.State, "error", err)
		}
	}
}

// send delivers one notification. Its errors never contain the URL: Discord
// and Slack webhook URLs carry their token.
func (n *Notifier) send(ctx context.Context, s sender, e Event) error {
	r, err := s.format(e)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(r.body))
	if err != nil {
		return errors.New("build request: invalid URL")
	}
	for k, v := range r.header {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", r.contentType)
	resp, err := n.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("send: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("answered %s", resp.Status)
	}
	return nil
}

// title is the one-line summary used by the readable adapters.
func title(e Event) string {
	switch store.State(e.State) {
	case store.StatePendingApproval:
		return e.Job + " waiting for approval"
	case store.StateFailed:
		return e.Job + " failed"
	}
	return e.Job + " " + e.State
}

// shortCommit is the first 12 characters of the commit SHA.
func shortCommit(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// text is the plain body used by ntfy and Gotify.
func text(e Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Namespace: %s\nCommit: %s", e.Namespace, shortCommit(e.Commit))
	if e.Error != "" {
		fmt.Fprintf(&b, "\nError: %s", e.Error)
	}
	if e.URL != "" {
		fmt.Fprintf(&b, "\n%s", e.URL)
	}
	return b.String()
}

func jsonRequest(v any) (request, error) {
	b, err := json.Marshal(v)
	return request{body: b, contentType: "application/json", header: http.Header{}}, err
}

func bearer(h http.Header, token string) {
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
}

func webhook(e Event, token string) (request, error) {
	r, err := jsonRequest(e)
	bearer(r.header, token)
	return r, err
}

// Discord limits, https://discord.com/developers/docs/resources/message#embed-object-embed-limits.
const (
	discordTitleMax       = 256
	discordDescriptionMax = 4096
	discordFieldMax       = 1024
	colorFailed           = 0xD03030
	colorPending          = 0xE0A000
)

func discord(e Event) (request, error) {
	type field struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Inline bool   `json:"inline"`
	}
	type embed struct {
		Title       string    `json:"title"`
		Description string    `json:"description,omitempty"`
		URL         string    `json:"url,omitempty"`
		Color       int       `json:"color"`
		Fields      []field   `json:"fields"`
		Timestamp   time.Time `json:"timestamp"`
	}
	color := colorPending
	if store.State(e.State) == store.StateFailed {
		color = colorFailed
	}
	return jsonRequest(struct {
		Username string  `json:"username"`
		Embeds   []embed `json:"embeds"`
	}{
		Username: "nops",
		Embeds: []embed{{
			Title:       truncate(title(e), discordTitleMax),
			Description: truncate(e.Error, discordDescriptionMax),
			URL:         e.URL,
			Color:       color,
			Fields: []field{
				{Name: "Namespace", Value: truncate(orDash(e.Namespace), discordFieldMax), Inline: true},
				{Name: "Commit", Value: orDash(shortCommit(e.Commit)), Inline: true},
			},
			Timestamp: e.Time,
		}},
	})
}

func slack(e Event) (request, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "*%s*\nNamespace: %s · Commit: `%s`", slackEscape(title(e)),
		slackEscape(e.Namespace), slackEscape(shortCommit(e.Commit)))
	if e.Error != "" {
		fmt.Fprintf(&b, "\n>%s", strings.ReplaceAll(slackEscape(e.Error), "\n", "\n>"))
	}
	if e.URL != "" {
		fmt.Fprintf(&b, "\n<%s|Open in nops>", e.URL)
	}
	return jsonRequest(struct {
		Text string `json:"text"`
	}{b.String()})
}

// slackEscape escapes the three characters Slack's mrkdwn gives a meaning.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// ntfy priorities: 3 is default, 4 is high.
func ntfy(e Event, token string) (request, error) {
	h := http.Header{}
	h.Set("Title", title(e))
	if store.State(e.State) == store.StateFailed {
		h.Set("Priority", "4")
		h.Set("Tags", "rotating_light")
	} else {
		h.Set("Priority", "3")
		h.Set("Tags", "hourglass")
	}
	if e.URL != "" {
		h.Set("Click", e.URL)
	}
	bearer(h, token)
	return request{body: []byte(text(e)), contentType: "text/plain; charset=utf-8", header: h}, nil
}

// Gotify priorities: 5 is default, 8 and above is high.
func gotify(e Event, token string) (request, error) {
	priority := 5
	if store.State(e.State) == store.StateFailed {
		priority = 8
	}
	msg := map[string]any{"title": title(e), "message": text(e), "priority": priority}
	if e.URL != "" {
		msg["extras"] = map[string]any{
			"client::notification": map[string]any{"click": map[string]string{"url": e.URL}},
		}
	}
	r, err := jsonRequest(msg)
	r.header.Set("X-Gotify-Key", token)
	return r, err
}

// truncate cuts s to at most max runes, ending with "…" when it cuts.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
