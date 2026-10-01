// Package notify tells people that a deployment needs them: it sends a
// notification to every configured adapter (generic webhook, Discord, Slack,
// ntfy, Gotify). A failed delivery is logged at WARN and never reaches the
// caller, so it cannot block the state machine.
//
// The adapters and their payloads are documented in
// docs/logs-and-notifications.md#notifications: keep the two aligned.
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
	"sync/atomic"
	"time"

	"github.com/music-gang/nops/internal/nomadx"
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
	// CommitURL returns the web address of a commit, or "" if there is none.
	CommitURL func(sha string) string
	// NomadUIURL is the address of the Nomad UI as a browser reaches it; empty
	// means no link to the job in Nomad.
	NomadUIURL string
	Timeout    time.Duration
}

// Endpoint is where an adapter sends, and with which token.
type Endpoint struct {
	URL   string
	Token string
}

// Event is what a notification says. It is also the body of the generic
// webhook.
type Event struct {
	DeploymentID string `json:"deployment_id"`
	Job          string `json:"job"`
	Namespace    string `json:"namespace"`
	State        string `json:"state"`
	// Waiting says what an applying deployment waits for a person to do:
	// WaitingCanaryPromotion. Empty for every other notification.
	Waiting string `json:"waiting,omitempty"`
	// Phase is where a failed deployment failed: detection, pre, apply or
	// post, as in the log. Empty for every other notification.
	Phase      string `json:"phase,omitempty"`
	Policy     string `json:"policy,omitempty"`
	ApprovedBy string `json:"approved_by,omitempty"`
	Error      string `json:"error"`
	Commit     string `json:"commit"`
	// CommitSubject and CommitAuthor are empty for a deployment created before
	// they were recorded; CommitURL when the repository is not an http(s) URL.
	CommitSubject string `json:"commit_subject,omitempty"`
	CommitAuthor  string `json:"commit_author,omitempty"`
	CommitURL     string `json:"commit_url,omitempty"`
	URL           string `json:"url"` // link to the deployment; empty without a public URL
	NomadURL      string `json:"nomad_url,omitempty"`
	// RetryOf is the ID of the failed or rejected deployment this one retries.
	RetryOf string `json:"retry_of,omitempty"`
	// Changes sums up the plan diff of a deployment waiting for approval, such
	// as "2 groups, 3 tasks changed".
	Changes string    `json:"changes,omitempty"`
	Time    time.Time `json:"time"`

	logo string // address of Nops's logo, for the adapters that show a sender
}

// WaitingCanaryPromotion is the Waiting of a deployment whose Nomad deployment
// waits for a person to promote its canaries.
const WaitingCanaryPromotion = "canary_promotion"

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
	failed *atomic.Uint64 // deliveries that did not go through, for Failures
}

// Notifier sends notifications. Build it with New.
type Notifier struct {
	senders   []sender
	publicURL string
	nomadUI   string
	commitURL func(sha string) string
	client    *http.Client
	log       *slog.Logger
}

// New builds a Notifier with one sender per adapter whose URL is set.
func New(o Options, log *slog.Logger) *Notifier {
	n := &Notifier{
		publicURL: strings.TrimRight(o.PublicURL, "/"),
		nomadUI:   strings.TrimRight(o.NomadUIURL, "/"),
		commitURL: o.CommitURL,
		client:    &http.Client{Timeout: o.Timeout},
		log:       log,
	}
	add := func(name, url string, format func(Event) (request, error)) {
		if url != "" {
			n.senders = append(n.senders, sender{name: name, url: url, format: format, failed: new(atomic.Uint64)})
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
// Call it after the transition to the notified state has been saved. phase is
// where a failed deployment failed, "" for any other.
func (n *Notifier) Notify(ctx context.Context, d *store.Deployment, phase string) {
	if len(n.senders) == 0 {
		return
	}
	e := Event{
		DeploymentID:  d.ID,
		Job:           d.JobID,
		Namespace:     d.Namespace,
		State:         string(d.State),
		Phase:         phase,
		Policy:        string(d.Policy),
		ApprovedBy:    d.DecidedBy,
		Error:         d.Error,
		Commit:        d.CommitSHA,
		CommitSubject: d.CommitSubject,
		CommitAuthor:  d.CommitAuthor,
		NomadURL:      nomadx.UIJobURL(n.nomadUI, d.Namespace, d.JobID),
		RetryOf:       d.RetryOf,
		Time:          d.UpdatedAt.UTC(),
		logo:          logoURL(n.publicURL),
	}
	if n.commitURL != nil {
		e.CommitURL = n.commitURL(d.CommitSHA)
	}
	if d.State == store.StatePendingApproval {
		e.Changes = n.changes(ctx, d)
	}
	if d.State == store.StateApplying && !d.PromotionWaitSince.IsZero() && d.PromotedAt.IsZero() {
		e.Waiting = WaitingCanaryPromotion
	}
	if n.publicURL != "" {
		e.URL = n.publicURL + "/deployments/" + url.PathEscape(d.ID)
	}
	for _, s := range n.senders {
		if err := n.send(ctx, s, e); err != nil {
			s.failed.Add(1)
			n.log.WarnContext(ctx, "notification not delivered",
				"adapter", s.name, "deployment_id", e.DeploymentID, "job", e.Job,
				"namespace", e.Namespace, "state", e.State, "error", err)
		}
	}
}

// Failures returns, for every configured adapter, how many notifications it
// failed to deliver since the start: a failed delivery is only a WARN in the
// log, so this is how a scrape of /metrics sees an adapter that stopped
// working (docs/metrics.md). An adapter that never failed is there with 0.
func (n *Notifier) Failures() map[string]uint64 {
	out := make(map[string]uint64, len(n.senders))
	for _, s := range n.senders {
		out[s.name] = s.failed.Load()
	}
	return out
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
	discordContentMax     = 2000
	discordTitleMax       = 256
	discordDescriptionMax = 4096
	discordFieldMax       = 1024
	colorFailed           = 0xD03030
	colorPending          = 0xE0A000
	// colorCompleted matches the dashboard's own "completed" colour
	// (docs/development.md#the-dashboards-code, --success in app.css).
	colorCompleted = 0x1A7F37
)

// discord sends the preview as the content, because the push notification of
// an embed-only message says little, and the body as an embed.
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
	switch store.State(e.State) {
	case store.StateFailed:
		color = colorFailed
	case store.StateCompleted:
		color = colorCompleted
	}

	sha := "`" + orDash(shortCommit(e.Commit)) + "`"
	if e.CommitURL != "" {
		sha = "[" + sha + "](" + e.CommitURL + ")"
	}
	fields := []field{{Name: "Commit", Value: truncate(strings.TrimSpace(sha+" "+discordEscape(commitLead(e))), discordFieldMax)}}
	for _, f := range facts(e) {
		fields = append(fields, field{Name: f.name, Value: truncate(discordEscape(f.value), discordFieldMax), Inline: true})
	}
	if e.Changes != "" {
		fields = append(fields, field{Name: "Changes", Value: e.Changes})
	}
	var ls []string
	for _, l := range links(e) {
		ls = append(ls, "["+l.label+"]("+l.url+")")
	}
	if len(ls) > 0 {
		fields = append(fields, field{Name: "Links", Value: truncate(strings.Join(ls, " · "), discordFieldMax)})
	}

	content := title(e)
	if l := lead(e); l != "" {
		content += "\n" + l
	}
	return jsonRequest(struct {
		Username        string         `json:"username"`
		AvatarURL       string         `json:"avatar_url,omitempty"`
		Content         string         `json:"content"`
		AllowedMentions map[string]any `json:"allowed_mentions"`
		Embeds          []embed        `json:"embeds"`
	}{
		Username:  "Nops",
		AvatarURL: e.logo,
		Content:   truncate(content, discordContentMax),
		// A commit subject or an error can hold @everyone: it must not ping.
		AllowedMentions: map[string]any{"parse": []string{}},
		Embeds: []embed{{
			Title:       truncate(title(e), discordTitleMax),
			Description: truncate(e.Error, discordDescriptionMax),
			URL:         e.URL,
			Color:       color,
			Fields:      fields,
			Timestamp:   e.Time,
		}},
	})
}

// discordEscape escapes what Discord's Markdown gives a meaning.
func discordEscape(s string) string {
	return strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "~", "\\~", "`", "\\`", "|", "\\|", ">", "\\>", "[", "\\[", "]", "\\]").Replace(s)
}

// Slack limits, https://api.slack.com/reference/block-kit/blocks.
const (
	slackHeaderMax  = 150
	slackSectionMax = 3000
)

// slack sends Block Kit for Slack, with the whole message also as the top-level
// text: Slack shows it in notifications, and the receivers that stand in for
// Slack (Mattermost, Rocket.Chat) ignore the blocks and show only it.
func slack(e Event) (request, error) {
	section := func(mrkdwn string) map[string]any {
		return map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": truncate(mrkdwn, slackSectionMax)}}
	}
	mrkdwn := func(s string) map[string]any { return map[string]any{"type": "mrkdwn", "text": s} }

	sha := "`" + slackEscape(orDash(shortCommit(e.Commit))) + "`"
	if e.CommitURL != "" {
		sha = "<" + e.CommitURL + "|" + sha + ">"
	}
	commit := strings.TrimSpace(sha + " " + slackEscape(commitLead(e)))
	var fs []string
	for _, f := range facts(e) {
		fs = append(fs, f.name+": "+slackEscape(f.value))
	}

	blocks := []any{
		map[string]any{"type": "header", "text": map[string]any{"type": "plain_text", "text": truncate(title(e), slackHeaderMax), "emoji": true}},
		section(commit),
		map[string]any{"type": "context", "elements": []any{mrkdwn(strings.Join(fs, " · "))}},
	}
	if e.Error != "" {
		blocks = append(blocks, section(">"+strings.ReplaceAll(truncate(slackEscape(e.Error), slackSectionMax-1), "\n", "\n>")))
	}
	if e.Changes != "" {
		blocks = append(blocks, section(slackEscape(e.Changes)))
	}
	var buttons []any
	var inline []string
	for _, l := range links(e) {
		buttons = append(buttons, map[string]any{"type": "button", "text": map[string]any{"type": "plain_text", "text": l.label}, "url": l.url})
		inline = append(inline, "<"+l.url+"|"+l.label+">")
	}
	if len(buttons) > 0 {
		blocks = append(blocks, map[string]any{"type": "actions", "elements": buttons})
	}

	var b strings.Builder
	fmt.Fprintf(&b, "*%s*", slackEscape(title(e)))
	l := lead(e)
	if l != "" {
		fmt.Fprintf(&b, "\n%s", slackEscape(l))
	}
	b.WriteString("\nCommit: " + sha)
	if d := commitDetail(e); d != "" {
		b.WriteString(" " + slackEscape(d))
	}
	fmt.Fprintf(&b, "\n%s", strings.Join(fs, " · "))
	if e.Error != "" && e.Error != l {
		fmt.Fprintf(&b, "\n>%s", strings.ReplaceAll(slackEscape(e.Error), "\n", "\n>"))
	}
	if e.Changes != "" {
		fmt.Fprintf(&b, "\n%s", slackEscape(e.Changes))
	}
	if len(inline) > 0 {
		fmt.Fprintf(&b, "\n%s", strings.Join(inline, " · "))
	}
	return jsonRequest(map[string]any{"text": b.String(), "blocks": blocks})
}

// slackEscape escapes the three characters Slack's mrkdwn gives a meaning.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// ntfyMessageMax is the longest message ntfy keeps as text, in bytes.
const ntfyMessageMax = 4096

// ntfy sends plain text, because ntfy renders Markdown only in its web app. The
// emoji of the title is a tag, which ntfy shows before the title, and the links
// are buttons on the notification. Priorities: 3 is default, 4 is high, 2 is
// low (completed needs no action, so it must not compete with a failure or an
// approval for attention).
func ntfy(e Event, token string) (request, error) {
	h := http.Header{}
	h.Set("Title", headline(e))
	h.Set("Tags", ntfyTag(e))
	switch store.State(e.State) {
	case store.StateFailed:
		h.Set("Priority", "4")
	case store.StateCompleted:
		h.Set("Priority", "2")
	default:
		h.Set("Priority", "3")
	}
	if e.URL != "" {
		h.Set("Click", e.URL)
	}
	if e.logo != "" {
		h.Set("Icon", e.logo)
	}
	var actions []string
	for _, l := range links(e) {
		actions = append(actions, "view, "+l.label+", "+l.url)
	}
	if len(actions) > 0 {
		h.Set("Actions", strings.Join(actions, "; "))
	}
	bearer(h, token)
	return request{body: []byte(truncateBytes(text(e, false), ntfyMessageMax)), contentType: "text/plain; charset=utf-8", header: h}, nil
}

// ntfyTag is the shortcode of the emoji of the title, from ntfy's own list.
func ntfyTag(e Event) string {
	if e.Waiting == WaitingCanaryPromotion {
		return "baby_chick"
	}
	switch store.State(e.State) {
	case store.StateFailed:
		return "x"
	case store.StateCompleted:
		return "white_check_mark"
	}
	return "hourglass_flowing_sand"
}

// Gotify priorities: 5 is default, 8 and above is high, 2 is low (completed
// needs no action).
func gotify(e Event, token string) (request, error) {
	priority := 5
	switch store.State(e.State) {
	case store.StateFailed:
		priority = 8
	case store.StateCompleted:
		priority = 2
	}
	msg := map[string]any{"title": title(e), "message": text(e, true), "priority": priority}
	if e.URL != "" {
		msg["extras"] = map[string]any{
			"client::notification": map[string]any{"click": map[string]string{"url": e.URL}},
		}
	}
	r, err := jsonRequest(msg)
	r.header.Set("X-Gotify-Key", token)
	return r, err
}

// changes sums up the plan diff of a deployment waiting for approval. A diff
// that does not decode is logged and leaves the notification without it: it
// must not keep the approval from reaching a person.
func (n *Notifier) changes(ctx context.Context, d *store.Deployment) string {
	diff, err := nomadx.ParseDiff(d.PlanDiff)
	if err != nil {
		n.log.WarnContext(ctx, "plan diff not summed up in the notification",
			"deployment_id", d.ID, "job", d.JobID, "namespace", d.Namespace, "error", err)
		return ""
	}
	return nomadx.Summarize(diff).Line()
}
