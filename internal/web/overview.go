package web

import (
	"net/http"
	"time"

	"github.com/music-gang/nops/internal/store"
)

// gitView is the repository nops reads, as the Overview's strip shows it.
type gitView struct {
	Known                  bool // there is a snapshot to describe
	SHA, Short, URL        string
	Subject, Author        string
	CommittedAt, CheckedAt timeView
	Error                  string // the last poll's failure, "" if it succeeded
	ErrorAt                timeView
	CanFetch               bool // POST /fetch is served
}

// cycleView is how the last detection cycle went.
type cycleView struct {
	Ran                        bool
	At                         timeView
	Took                       string
	Error                      string
	Managed, Skipped, Unparsed int
	// OrphanCheckSkipped: jobs removed from git were not looked for this cycle.
	OrphanCheckSkipped bool
}

type overviewData struct {
	baseData
	Git        gitView
	Cycle      cycleView
	Attention  []attentionItem
	InProgress []deploymentCard // detected, pre_hook, applying, post_hook
}

func (s *server) gitView() gitView {
	snap, st := s.git.Snapshot(), s.git.Status()
	v := gitView{
		Known: snap.Commit != "", SHA: snap.Commit, Short: shortCommit(snap.Commit),
		Subject: snap.Subject, Author: snap.Author, CommittedAt: s.when(snap.CommittedAt),
		CheckedAt: s.when(st.CheckedAt), Error: st.Error, ErrorAt: s.when(st.ErrorAt), CanFetch: s.trigger != nil,
	}
	if s.commitURL != nil {
		v.URL = s.commitURL(snap.Commit)
	}
	return v
}

func (s *server) cycleView() cycleView {
	st := s.engine.Status()
	return cycleView{
		Ran: !st.At.IsZero(), At: s.when(st.At), Took: st.Duration.Round(time.Millisecond).String(), Error: st.Error,
		Managed: st.Managed, Skipped: st.Skipped, Unparsed: st.Unparsed, OrphanCheckSkipped: st.OrphanCheckSkipped,
	}
}

// index is GET /: what needs a person, what is moving, and whether nops
// itself is working.
func (s *server) index(w http.ResponseWriter, r *http.Request) {
	active, err := s.store.ListActive(r.Context())
	if err != nil {
		s.serverError(w, r, "list active deployments", err)
		return
	}
	latest, err := s.latestByJob(r)
	if err != nil {
		s.serverError(w, r, "list latest deployments", err)
		return
	}
	data := overviewData{
		baseData:  s.base(r, "overview"),
		Git:       s.gitView(),
		Cycle:     s.cycleView(),
		Attention: s.attention(s.engine.Observations(), active, latest),
	}
	for _, d := range active {
		if d.State != store.StatePendingApproval {
			data.InProgress = append(data.InProgress, s.card(d))
		}
	}
	s.render(w, r, "overview", data)
}
