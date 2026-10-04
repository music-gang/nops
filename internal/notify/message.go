package notify

import (
	"strings"
	"unicode/utf8"

	"github.com/music-gang/nops/internal/store"
)

// Every adapter says the same things in the same order: a title and a lead
// line, which are the preview a system notification shows, then the commit,
// the context, the error, the changes and the links. The helpers below build
// each part once; an adapter only formats it.

const (
	// leadMax keeps the preview line short, so a lock screen does not cut it.
	leadMax  = 200
	logoPath = "/static/logo-192x192.png"
)

// phaseLabels say where a failed deployment failed, from the phase the engine
// gives (and the log shows).
var phaseLabels = map[string]string{
	"detection": "detection",
	"pre":       "the pre-hook",
	"apply":     "the apply",
	"post":      "the post-hook",
}

// emoji is the same in every adapter.
func emoji(e Event) string {
	if e.Waiting == WaitingCanaryPromotion {
		return "🐤"
	}
	switch store.State(e.State) {
	case store.StatePendingApproval:
		return "⏳"
	case store.StateFailed:
		return "❌"
	case store.StateCompleted:
		return "✅"
	}
	return ""
}

// headline says what happened and, for a failure, where.
func headline(e Event) string {
	if e.Waiting == WaitingCanaryPromotion {
		return e.Job + " canaries need promotion"
	}
	switch store.State(e.State) {
	case store.StatePendingApproval:
		return e.Job + " needs approval"
	case store.StateFailed:
		if label, ok := phaseLabels[e.Phase]; ok {
			return e.Job + " failed in " + label
		}
		return e.Job + " failed"
	case store.StateCompleted:
		return e.Job + " completed"
	}
	return e.Job + " " + e.State
}

// title is the headline with its emoji.
func title(e Event) string {
	if em := emoji(e); em != "" {
		return em + " " + headline(e)
	}
	return headline(e)
}

// lead is the first line of the body, which a system notification shows under
// the title: what to do next, or the commit or the error that matters.
func lead(e Event) string {
	if e.Waiting == WaitingCanaryPromotion {
		return "promote them in Nomad"
	}
	switch store.State(e.State) {
	case store.StateFailed:
		first, _, _ := strings.Cut(strings.TrimSpace(e.Error), "\n")
		return truncate(strings.TrimSpace(first), leadMax)
	case store.StatePendingApproval, store.StateCompleted:
		if l := commitLead(e); l != "" {
			return truncate(l, leadMax)
		}
		return shortCommit(e.Commit)
	}
	return ""
}

// commitLead is the subject and the author of the commit.
func commitLead(e Event) string {
	var parts []string
	for _, p := range []string{e.CommitSubject, e.CommitAuthor} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " · ")
}

// commitDetail is what the commit line adds to the lead: nothing when the lead
// already is the commit.
func commitDetail(e Event) string {
	if cl := commitLead(e); cl != lead(e) {
		return cl
	}
	return ""
}

// shortCommit is the first 12 characters of the commit SHA.
func shortCommit(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

type fact struct{ name, value string }

// facts are the context of the deployment: where it runs, how it is applied,
// who approved it, what it retries and who deployed it now. A fact with no
// value is left out, but the namespace.
func facts(e Event) []fact {
	out := []fact{{"Namespace", orDash(e.Namespace)}}
	for _, f := range []fact{{"Policy", e.Policy}, {"Approved by", e.ApprovedBy}, {"Retry of", e.RetryOf}, {"Deployed now by", e.DeployedNowBy}} {
		if f.value != "" {
			out = append(out, f)
		}
	}
	return out
}

type link struct{ label, url string }

// links are where a person goes next. A link whose URL is not configured is
// left out.
func links(e Event) []link {
	var out []link
	for _, l := range []link{{"Open in Nops", e.URL}, {"Commit", e.CommitURL}, {"Open in Nomad", e.NomadURL}} {
		if l.url != "" {
			out = append(out, l)
		}
	}
	return out
}

// text is the plain body of ntfy and Gotify: a notification renders no markup
// there. ntfy shows the links as buttons, so it leaves them out of the text.
func text(e Event, withLinks bool) string {
	var lines []string
	l := lead(e)
	if l != "" {
		lines = append(lines, l)
	}
	commit := "Commit: " + orDash(shortCommit(e.Commit))
	if d := commitDetail(e); d != "" {
		commit += " " + d
	}
	lines = append(lines, commit)
	var fs []string
	for _, f := range facts(e) {
		fs = append(fs, f.name+": "+f.value)
	}
	lines = append(lines, strings.Join(fs, " · "))
	if e.Error != "" && e.Error != l {
		lines = append(lines, "Error: "+e.Error)
	}
	if e.Changes != "" {
		lines = append(lines, e.Changes)
	}
	if withLinks {
		for _, k := range links(e) {
			lines = append(lines, k.label+": "+k.url)
		}
	}
	return strings.Join(lines, "\n")
}

// truncate cuts s to at most max runes, ending with "…" when it cuts.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// truncateBytes cuts s to at most max bytes, on a character boundary, ending
// with "…" when it cuts.
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// logoURL is the address of Nops's logo, the dashboard's own, or "" without a
// public URL.
func logoURL(publicURL string) string {
	if publicURL == "" {
		return ""
	}
	return publicURL + logoPath
}
