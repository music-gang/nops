package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
)

// webhookMaxBody bounds how much of a webhook request nops reads: it never
// looks at the payload beyond checking its signature, so there is no reason
// to buffer more than a generous margin over what any forge sends for a
// single push event.
const webhookMaxBody = 1 << 20 // 1 MiB

// webhook authenticates a git forge's push notification and asks the
// watcher for an out-of-turn poll. It never inspects the payload otherwise:
// a valid signature is enough to mean "something changed, go look"
// (docs/roadmap.md#web). Each forge signs differently:
//   - GitHub:  X-Hub-Signature-256: sha256=<hex hmac>
//   - Gitea:   X-Gitea-Signature:   <hex hmac>            (same construction)
//   - GitLab:  X-Gitlab-Token:      <the secret itself>
//
// A request matching none of them is refused with 401 and a WARN log; it
// never says which check failed, so a probe cannot learn which forge nops
// expects.
func (s *server) webhook(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, webhookMaxBody)
	if tok := r.Header.Get("X-Gitlab-Token"); tok != "" {
		if !equal(tok, string(s.secret)) {
			s.webhookRejected(w, r, "gitlab")
			return
		}
		io.Copy(io.Discard, r.Body)
		s.webhookAccepted(w, r)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.log.WarnContext(r.Context(), "webhook: reading the body failed", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if sig := r.Header.Get("X-Hub-Signature-256"); sig != "" {
		if !verifyHMACSHA256(sig, "sha256=", body, s.secret) {
			s.webhookRejected(w, r, "github")
			return
		}
		s.webhookAccepted(w, r)
		return
	}
	if sig := r.Header.Get("X-Gitea-Signature"); sig != "" {
		if !verifyHMACSHA256(sig, "", body, s.secret) {
			s.webhookRejected(w, r, "gitea")
			return
		}
		s.webhookAccepted(w, r)
		return
	}

	s.log.WarnContext(r.Context(), "webhook: no recognized signature header")
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func (s *server) webhookAccepted(w http.ResponseWriter, r *http.Request) {
	s.trigger()
	w.WriteHeader(http.StatusAccepted)
}

func (s *server) webhookRejected(w http.ResponseWriter, r *http.Request, forge string) {
	s.log.WarnContext(r.Context(), "webhook: signature does not match", "forge", forge)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// verifyHMACSHA256 checks header against the hex HMAC-SHA256 of body with
// secret, after dropping prefix (GitHub's "sha256="; Gitea has none).
func verifyHMACSHA256(header, prefix string, body, secret []byte) bool {
	sigHex := header
	if prefix != "" {
		if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
			return false
		}
		sigHex = header[len(prefix):]
	}
	want, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}
