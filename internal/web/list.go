package web

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/music-gang/nops/internal/acl"
	"github.com/music-gang/nops/internal/meta"
	"github.com/music-gang/nops/internal/store"
)

// The lists of deployments and jobs (docs/api.md#lists): one parser of their
// parameters for the dashboard and the API, pages by cursor, and the counts of
// the filters.

// perPages are the sizes a page may have; defaultPerPage is the one without a
// choice.
var perPages = []int{25, 50, 100}

const defaultPerPage = 50

// perPageCookie keeps the size a person picked on the dashboard, for the lists
// whose URL names none.
const perPageCookie = "nops_per_page"

// maxCount is where the count of a filter of deployments stops: it walks at
// most that many rows, and shows "1000+" past it.
const maxCount = 1000

// listKind is which list a query is for: the parameters differ.
type listKind int

const (
	deploymentList listKind = iota
	jobList
)

// deploymentStates are the states a list of deployments filters on: "active",
// every state that is not terminal, and each state.
var deploymentStates = []store.State{
	store.StateDetected, store.StatePendingApproval, store.StatePreHook, store.StateApplying, store.StatePostHook,
	store.StateCompleted, store.StateFailed, store.StateRejected, store.StateSuperseded,
}

// listQuery is the parameters of a list, checked.
type listQuery struct {
	PerPage int
	// perPageSet says the URL named the size: the dashboard keeps it.
	perPageSet bool
	Cursor     *cursor
	Search     string
	State      string
	Namespace  string
	Job        string
	Repository string
	Policy     string
	DecidedBy  string
	// Since and Until are the bounds as given, for the form; since and until
	// are the times they stand for.
	Since, Until string
	since, until time.Time
	Warnings     bool
}

// paramsOf are the parameters that belong to only one list.
var paramsOf = map[listKind][]string{
	deploymentList: {"job", "decided_by", "since", "until"},
	jobList:        {"warnings"},
}

// parseList checks the parameters of a list. The error is for the person who
// sent them.
func parseList(v url.Values, kind listKind) (listQuery, error) {
	q := listQuery{PerPage: defaultPerPage}
	other := deploymentList
	if kind == deploymentList {
		other = jobList
	}
	for _, p := range paramsOf[other] {
		if v.Has(p) {
			return q, fmt.Errorf("%s does not filter this list", p)
		}
	}
	if s := v.Get("per_page"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || !slices.Contains(perPages, n) {
			return q, errors.New("per_page is 25, 50 or 100")
		}
		q.PerPage, q.perPageSet = n, true
	}
	if t := v.Get("next_token"); t != "" {
		c, err := parseCursor(t, kind)
		if err != nil {
			return q, err
		}
		q.Cursor = c
	}
	q.Search = strings.TrimSpace(v.Get("q"))
	if kind == deploymentList && q.Search != "" && len([]rune(q.Search)) < store.MinSearch {
		return q, fmt.Errorf("q needs at least %d characters", store.MinSearch)
	}
	q.State = v.Get("state")
	switch {
	case q.State == "":
	case kind == deploymentList:
		if q.State != "active" && !slices.Contains(deploymentStates, store.State(q.State)) {
			return q, fmt.Errorf("state %q is not active or a state of a deployment", q.State)
		}
	case !syncKey(q.State).valid():
		return q, fmt.Errorf("state %q is not a sync state", q.State)
	}
	q.Namespace, q.Job, q.Repository, q.DecidedBy = v.Get("namespace"), v.Get("job"), v.Get("repository"), v.Get("decided_by")
	switch q.Policy = v.Get("policy"); meta.Policy(q.Policy) {
	case "", meta.PolicyAuto, meta.PolicyApproval, meta.PolicyNone:
	default:
		return q, fmt.Errorf("policy %q is not auto, approval or none", q.Policy)
	}
	var err error
	q.Since, q.Until = v.Get("since"), v.Get("until")
	if q.since, err = parseBound(q.Since, false); err != nil {
		return q, fmt.Errorf("since: %w", err)
	}
	if q.until, err = parseBound(q.Until, true); err != nil {
		return q, fmt.Errorf("until: %w", err)
	}
	if s := v.Get("warnings"); s != "" {
		if q.Warnings, err = strconv.ParseBool(s); err != nil {
			return q, errors.New("warnings is true or false")
		}
	}
	return q, nil
}

// parseBound reads a date (a UTC day, the whole of it for until) or an RFC
// 3339 time.
func parseBound(s string, until bool) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.Parse(time.DateOnly, s); err == nil {
		if until {
			d = d.AddDate(0, 0, 1)
		}
		return d, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a date (2026-10-01) or an RFC 3339 time", s)
	}
	return t, nil
}

// deployments is the store's query of q's filters, for the reader's scope.
func (q listQuery) deployments(read acl.Scope) store.DeploymentQuery {
	dq := store.DeploymentQuery{
		Read: read, Search: q.Search, Namespace: q.Namespace, JobID: q.Job, Repository: q.Repository,
		DecidedBy: q.DecidedBy, Policy: store.Policy(q.Policy), Since: q.since, Until: q.until,
	}
	switch q.State {
	case "":
	case "active":
		dq.Active = true
	default:
		dq.States = []store.State{store.State(q.State)}
	}
	return dq
}

// -- the cursor ----------------------------------------------------------------

// cursor is where a page starts: after a row, towards older rows, or before
// it, towards newer ones. Its text is opaque to whoever holds it.
type cursor struct {
	newer bool
	// pos is the row of a list of deployments; key the one of a list of jobs.
	pos store.Position
	key string
}

// String encodes the cursor.
func (c cursor) String() string {
	dir := "o"
	if c.newer {
		dir = "n"
	}
	s := "j" + dir + c.key
	if c.key == "" {
		s = "d" + dir + c.pos.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.pos.ID
	}
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

var errCursor = errors.New("next_token is not one this list gave")

func parseCursor(s string, kind listKind) (*cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) < 3 {
		return nil, errCursor
	}
	text := string(b)
	c := &cursor{newer: text[1] == 'n'}
	if text[1] != 'n' && text[1] != 'o' {
		return nil, errCursor
	}
	switch {
	case text[0] == 'j' && kind == jobList:
		c.key = text[2:]
	case text[0] == 'd' && kind == deploymentList:
		at, id, ok := strings.Cut(text[2:], "|")
		t, err := time.Parse(time.RFC3339Nano, at)
		if !ok || err != nil || id == "" {
			return nil, errCursor
		}
		c.pos = store.Position{CreatedAt: t, ID: id}
	default:
		return nil, errCursor
	}
	return c, nil
}

// page is what a list answers besides its rows: the cursors of the pages
// next to it. FirstNewer says the newer page is the first one, which takes no
// cursor.
type page struct {
	Older, Newer *cursor
	FirstNewer   bool
}

// pageDeployments lists the page of dq that q's cursor and size name. A newer
// page that reaches the newest row is the first page, full.
func (s *server) pageDeployments(ctx context.Context, dq store.DeploymentQuery, q listQuery) ([]*store.Deployment, page, error) {
	dq.Limit = q.PerPage
	c := q.Cursor
	if c != nil && c.newer {
		dq.After, dq.Newer = &c.pos, true
		list, more, err := s.store.ListDeployments(ctx, dq)
		if err != nil {
			return nil, page{}, err
		}
		if more {
			return list, page{
				Newer: &cursor{newer: true, pos: store.PositionOf(list[0])},
				Older: &cursor{pos: store.PositionOf(list[len(list)-1])},
			}, nil
		}
		dq.After, dq.Newer, c = nil, false, nil
	} else if c != nil {
		dq.After = &c.pos
	}
	list, more, err := s.store.ListDeployments(ctx, dq)
	if err != nil {
		return nil, page{}, err
	}
	var p page
	if more {
		p.Older = &cursor{pos: store.PositionOf(list[len(list)-1])}
	}
	switch {
	case c != nil && len(list) > 0:
		p.Newer = &cursor{newer: true, pos: store.PositionOf(list[0])}
	case c != nil:
		p.FirstNewer = true
	}
	return list, p, nil
}

// pageKeys picks the page of q among keys, sorted: the indexes of its rows.
func pageKeys(keys []string, q listQuery) (from, to int, p page) {
	n := q.PerPage
	c := q.Cursor
	if c != nil && c.newer {
		end, _ := slices.BinarySearch(keys, c.key)
		if end > n {
			from, to = end-n, end
			return from, to, page{Newer: &cursor{newer: true, key: keys[from]}, Older: &cursor{key: keys[to-1]}}
		}
		c = nil
	}
	if c != nil {
		from, _ = slices.BinarySearch(keys, c.key)
		if from < len(keys) && keys[from] == c.key {
			from++
		}
	}
	to = min(from+n, len(keys))
	if to < len(keys) {
		p.Older = &cursor{key: keys[to-1]}
	}
	switch {
	case c != nil && to > from:
		p.Newer = &cursor{newer: true, key: keys[from]}
	case c != nil:
		p.FirstNewer = true
	}
	return from, to, p
}

// -- the dashboard ---------------------------------------------------------------

// dashboardList parses the list of a page and keeps its size: a size in the URL
// sets the cookie, and the cookie sets the size of a URL that names none.
func (s *server) dashboardList(w http.ResponseWriter, r *http.Request, kind listKind) (listQuery, error) {
	q, err := parseList(r.URL.Query(), kind)
	if err != nil {
		return q, err
	}
	if q.perPageSet {
		path := s.basePath
		if path == "" {
			path = "/"
		}
		http.SetCookie(w, &http.Cookie{
			Name: perPageCookie, Value: strconv.Itoa(q.PerPage), Path: path, MaxAge: int((365 * 24 * time.Hour).Seconds()),
			HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
		})
	} else if n, err := strconv.Atoi(cookieValue(r, perPageCookie)); err == nil && slices.Contains(perPages, n) {
		q.PerPage = n
	}
	return q, nil
}

// listLink is this page's URL with the parameters of set changed: an empty
// value removes one. A link never carries a cursor it does not set.
func (s *server) listLink(r *http.Request, set map[string]string) string {
	v := r.URL.Query()
	v.Del("next_token")
	for k, val := range set {
		if val == "" {
			v.Del(k)
		} else {
			v.Set(k, val)
		}
	}
	link := s.basePath + r.URL.Path
	if enc := v.Encode(); enc != "" {
		link += "?" + enc
	}
	return link
}

// listView is what a list shows around its rows: the search, the menus of its
// filters, and, at the bottom, the links to the pages next to it and the sizes
// of a page.
type listView struct {
	listQuery
	// Path is where the search goes; Live the region of the page it refreshes
	// as the person types, while the search box, outside it, keeps the focus.
	Path, Live  string
	Placeholder string
	// Keep are the parameters a new search keeps: the filters chosen.
	Keep  []param
	Menus []menuView
	Sizes []sizeLink
	// Newer and Older are links, "" when there is no such page, with their
	// words: Newer and Older for deployments, Previous and Next for jobs.
	Newer, Older           string
	NewerLabel, OlderLabel string
	Error                  string // what is wrong with the parameters
}

type param struct{ Name, Value string }

// menuView is a filter of a list as a menu of links: it works without
// JavaScript, and a link shows what its sender saw.
type menuView struct {
	Label  string // "Policy"
	Chosen string // "" for any
	Items  []menuItem
}

type menuItem struct {
	Label, Href string
	Selected    bool
}

// sizeLink is one of the sizes of a page, as a link.
type sizeLink struct {
	N        int
	Href     string
	Selected bool
}

func (s *server) listView(r *http.Request, q listQuery, p page, live, placeholder string) listView {
	v := listView{
		listQuery: q, Path: s.basePath + r.URL.Path, Live: live, Placeholder: placeholder,
		NewerLabel: "Newer", OlderLabel: "Older",
	}
	for name, vals := range r.URL.Query() {
		if name != "q" && name != "next_token" && len(vals) > 0 && vals[0] != "" {
			v.Keep = append(v.Keep, param{name, vals[0]})
		}
	}
	slices.SortFunc(v.Keep, func(a, b param) int { return strings.Compare(a.Name, b.Name) })
	if p.Older != nil {
		v.Older = s.listLink(r, map[string]string{"next_token": p.Older.String()})
	}
	switch {
	case p.Newer != nil:
		v.Newer = s.listLink(r, map[string]string{"next_token": p.Newer.String()})
	case p.FirstNewer:
		v.Newer = s.listLink(r, nil)
	}
	// The sizes show only where they change something: with more than one
	// page, or a size that is not the default to go back from.
	if v.Newer != "" || v.Older != "" || q.PerPage != defaultPerPage {
		for _, n := range perPages {
			v.Sizes = append(v.Sizes, sizeLink{N: n, Href: s.listLink(r, map[string]string{"per_page": strconv.Itoa(n)}), Selected: n == q.PerPage})
		}
	}
	return v
}

// menu is the filter of a parameter as a menu: "Any" then each value. With no
// value to choose from, there is no menu.
func (s *server) menu(r *http.Request, label, name string, values []string, chosen string) []menuView {
	if len(values) == 0 {
		return nil
	}
	m := menuView{Label: label, Chosen: chosen, Items: []menuItem{{Label: "Any", Href: s.listLink(r, map[string]string{name: ""}), Selected: chosen == ""}}}
	for _, v := range values {
		m.Items = append(m.Items, menuItem{Label: v, Href: s.listLink(r, map[string]string{name: v}), Selected: v == chosen})
	}
	return []menuView{m}
}

// countLabel shows a count of maxCount+1 as "1000+".
func countLabel(n int) string {
	if n > maxCount {
		return strconv.Itoa(maxCount) + "+"
	}
	return strconv.Itoa(n)
}

// repositoryNames are the repositories nops reads, when it reads more than one:
// with one, there is nothing to choose.
func (s *server) repositoryNames() []string {
	repos := s.git.Repositories()
	if len(repos) < 2 {
		return nil
	}
	out := make([]string, len(repos))
	for i, r := range repos {
		out[i] = r.Name
	}
	return out
}
