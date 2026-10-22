package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/acl"
	"github.com/music-gang/nops/internal/gitwatch"
	"github.com/music-gang/nops/internal/store"
)

var everyNamespace = acl.Management().Scope(acl.Read)

func TestParseList(t *testing.T) {
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		query string
		kind  listKind
		check func(listQuery) bool
		err   string
	}{
		"defaults":       {"", deploymentList, func(q listQuery) bool { return q.PerPage == 50 && !q.perPageSet && q.Cursor == nil }, ""},
		"a size":         {"per_page=100", jobList, func(q listQuery) bool { return q.PerPage == 100 && q.perPageSet }, ""},
		"a size of ours": {"per_page=10", deploymentList, nil, "per_page is 25, 50 or 100"},
		"active":         {"state=active", deploymentList, func(q listQuery) bool { return q.deployments(everyNamespace).Active }, ""},
		"a state": {"state=pending_approval", deploymentList, func(q listQuery) bool {
			return slices.Equal(q.deployments(everyNamespace).States, []store.State{store.StatePendingApproval})
		}, ""},
		"a sync state":       {"state=drift", jobList, func(q listQuery) bool { return q.State == "drift" }, ""},
		"not a sync state":   {"state=failed", jobList, nil, "not a sync state"},
		"not a state":        {"state=drift", deploymentList, nil, "not active or a state"},
		"a policy":           {"policy=none", jobList, func(q listQuery) bool { return q.Policy == "none" }, ""},
		"not a policy":       {"policy=yolo", jobList, nil, "not auto, approval or none"},
		"a short search":     {"q=ab", deploymentList, nil, "at least 3 characters"},
		"a short job search": {"q=ab", jobList, func(q listQuery) bool { return q.Search == "ab" }, ""},
		"a day":              {"since=2026-10-01&until=2026-10-01", deploymentList, func(q listQuery) bool { return q.since.Equal(day) && q.until.Equal(day.AddDate(0, 0, 1)) }, ""},
		"a time":             {"until=2026-10-01T12:00:00Z", deploymentList, func(q listQuery) bool { return q.until.Equal(day.Add(12 * time.Hour)) }, ""},
		"not a time":         {"since=yesterday", deploymentList, nil, "since:"},
		"warnings":           {"warnings=true", jobList, func(q listQuery) bool { return q.Warnings }, ""},
		"a job's filter":     {"warnings=true", deploymentList, nil, "warnings does not filter this list"},
		"a deployment's":     {"decided_by=alice", jobList, nil, "decided_by does not filter this list"},
		"a token of another": {"next_token=" + cursor{key: "default/web"}.String(), deploymentList, nil, "next_token"},
		"not a token":        {"next_token=!!", jobList, nil, "next_token"},
	} {
		t.Run(name, func(t *testing.T) {
			v, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			q, err := parseList(v, tc.kind)
			switch {
			case tc.err != "":
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Errorf("error %v, want one with %q", err, tc.err)
				}
			case err != nil:
				t.Errorf("error %v", err)
			case !tc.check(q):
				t.Errorf("query %+v is not what %q says", q, tc.query)
			}
		})
	}
}

func TestCursorRoundTrips(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 123456789, time.UTC)
	for _, c := range []cursor{
		{pos: store.Position{CreatedAt: at, ID: "01J"}},
		{newer: true, pos: store.Position{CreatedAt: at, ID: "01J"}},
		{key: "default/web"},
		{newer: true, key: "prod/a|b"},
	} {
		kind := deploymentList
		if c.key != "" {
			kind = jobList
		}
		got, err := parseCursor(c.String(), kind)
		if err != nil || got.newer != c.newer || got.key != c.key || got.pos.ID != c.pos.ID || !got.pos.CreatedAt.Equal(c.pos.CreatedAt) {
			t.Errorf("cursor %+v came back as %+v, %v", c, got, err)
		}
	}
}

func TestPageKeys(t *testing.T) {
	keys := []string{"a", "b", "c", "d", "e"}
	q := listQuery{PerPage: 2}
	from, to, p := pageKeys(keys, q)
	if from != 0 || to != 2 || p.Older == nil || p.Older.key != "b" || p.Newer != nil || p.FirstNewer {
		t.Fatalf("first page = [%d, %d) %+v", from, to, p)
	}
	q.Cursor = p.Older
	if from, to, p = pageKeys(keys, q); from != 2 || to != 4 || p.Newer == nil || p.Newer.key != "c" {
		t.Fatalf("second page = [%d, %d) %+v", from, to, p)
	}
	q.Cursor = p.Older
	if from, to, p = pageKeys(keys, q); from != 4 || to != 5 || p.Older != nil {
		t.Fatalf("last page = [%d, %d) %+v, want no older", from, to, p)
	}
	q.Cursor = p.Newer
	if from, to, p = pageKeys(keys, q); from != 2 || to != 4 || p.Newer == nil {
		t.Fatalf("back one page = [%d, %d) %+v", from, to, p)
	}
	q.Cursor = p.Newer
	if from, to, p = pageKeys(keys, q); from != 0 || to != 2 || p.Newer != nil || p.FirstNewer {
		t.Fatalf("back to the first page = [%d, %d) %+v, want it whole and nothing newer", from, to, p)
	}
}

// many is n completed deployments of web, d000 the newest.
func many(n int) []*store.Deployment {
	out := make([]*store.Deployment, n)
	for i := range out {
		out[i] = dep(fmt.Sprintf("d%03d", i), "web", store.StateCompleted, time.Duration(i+1)*time.Minute)
	}
	return out
}

// Following next_token walks every deployment once and ends when it is absent,
// whatever the size of the last page.
func TestAPIDeploymentsPages(t *testing.T) {
	ts := newTestServer(t, &fakeStore{history: many(60)}, &fakeEngine{}, "")
	token := apiToken(t, ts, "alice", time.Time{})

	var seen []string
	target := "/api/deployments?per_page=25"
	for pages := 0; ; pages++ {
		if pages > 3 {
			t.Fatal("the pages never end")
		}
		rec := ts.api("GET", target, token, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d: %s", target, rec.Code, rec.Body)
		}
		var got struct {
			Deployments []struct{ ID string }
			NextToken   *string `json:"next_token"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		for _, d := range got.Deployments {
			seen = append(seen, d.ID)
		}
		if got.NextToken == nil {
			break
		}
		target = "/api/deployments?per_page=25&next_token=" + url.QueryEscape(*got.NextToken)
	}
	want := make([]string, 60)
	for i := range want {
		want[i] = fmt.Sprintf("d%03d", i)
	}
	if !slices.Equal(seen, want) {
		t.Errorf("walked %v, want d000 to d059 once, newest first", seen)
	}

	if rec := ts.api("GET", "/api/deployments?q=ab", token, ""); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "at least 3 characters") {
		t.Errorf("a short search: status %d, %s", rec.Code, rec.Body)
	}
	if rec := ts.api("GET", "/api/deployments?state=failed", token, ""); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "d0") {
		t.Errorf("a filter with no row: status %d, %s", rec.Code, rec.Body)
	}
}

func TestAPIJobsPages(t *testing.T) {
	st, en := jobsFixture()
	ts := newTestServer(t, st, en, "")
	token := apiToken(t, ts, "alice", time.Time{})

	var seen []string
	target := "/api/jobs?per_page=25&namespace=default"
	rec := ts.api("GET", target, token, "")
	var got struct {
		Jobs      []struct{ Namespace, Job string }
		NextToken string `json:"next_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("status %d, %v: %s", rec.Code, err, rec.Body)
	}
	for _, j := range got.Jobs {
		seen = append(seen, j.Job)
	}
	if !slices.IsSorted(seen) || len(seen) != 5 || got.NextToken != "" {
		t.Errorf("jobs = %v, next %q: want the 5 jobs in order and no next page", seen, got.NextToken)
	}
	rec = ts.api("GET", "/api/jobs?state=drift&q=BAT", token, "")
	if !strings.Contains(rec.Body.String(), `"job":"batch"`) || strings.Contains(rec.Body.String(), `"job":"web"`) {
		t.Errorf("search and filter: %s", rec.Body)
	}
	if rec := ts.api("GET", "/api/jobs?job=web", token, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("a filter of deployments on jobs: status %d, want 400", rec.Code)
	}
}

// The dashboard pages with links that work without JavaScript: Older from the
// first page, Newer back, and a size that stays in a cookie.
func TestActivityPages(t *testing.T) {
	ts := newTestServer(t, &fakeStore{history: many(60)}, &fakeEngine{}, "")
	cookie := ts.session("alice")

	rec := ts.do("GET", "/history?per_page=25", nil, cookie)
	page := rec.Body.String()
	mustContain(t, page, `href="/deployments/d024"`, `rel="next"`, `aria-disabled="true">Newer`)
	mustNotContain(t, page, `href="/deployments/d025"`)
	var size *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == perPageCookie {
			size = c
		}
	}
	if size == nil || size.Value != "25" {
		t.Fatalf("no size cookie: %v", rec.Result().Cookies())
	}

	older := link(t, page, "next")
	page = ts.do("GET", older, nil, cookie, size).Body.String()
	mustContain(t, page, `href="/deployments/d025"`, `href="/deployments/d049"`, `rel="prev"`, `option value="25" selected`)
	mustNotContain(t, page, `href="/deployments/d024"`, `href="/deployments/d050"`)

	newer := link(t, page, "prev")
	page = ts.do("GET", newer, nil, cookie, size).Body.String()
	mustContain(t, page, `href="/deployments/d000"`, `href="/deployments/d024"`, `aria-disabled="true">Newer`)

	// The size of the URL wins over the cookie's.
	page = ts.do("GET", "/history?per_page=50", nil, cookie, size).Body.String()
	mustContain(t, page, `href="/deployments/d049"`)
}

// link is the href of the pager's link with that rel.
func link(t *testing.T, page, rel string) string {
	t.Helper()
	before, _, ok := strings.Cut(page, `rel="`+rel+`"`)
	if !ok {
		t.Fatalf("no %s link in the page", rel)
	}
	i := strings.LastIndex(before, `href="`)
	href := before[i+len(`href="`):]
	href = href[:strings.Index(href, `"`)]
	return strings.ReplaceAll(href, "&amp;", "&")
}

func TestActivitySearchAndFilters(t *testing.T) {
	web := dep("d1", "web", store.StateCompleted, time.Hour)
	db := dep("d2", "db", store.StateFailed, 2*time.Hour)
	db.Namespace, db.Repository, db.DecidedBy = "prod", "infra", "alice"
	st := &fakeStore{history: []*store.Deployment{web, db}}
	ts := newTestServer(t, st, &fakeEngine{}, "")
	ts.git.more = []gitwatch.Repo{{Name: "infra"}}

	page := ts.get("/history?namespace=prod&decided_by=alice")
	mustContain(t, page, `href="/deployments/d2"`, `value="prod"`, `infra (1)`, `approval (1)`)
	mustNotContain(t, page, `href="/deployments/d1"`)

	page = ts.get("/history?q=zzz&state=failed")
	mustContain(t, page, "No deployment matches this filter.", `is-active" href="/history?q=zzz&amp;state=failed"`)
}

func TestJobPagePagesItsDeployments(t *testing.T) {
	st := &fakeStore{byJob: many(30)}
	ts := newTestServer(t, st, &fakeEngine{}, "")
	page := ts.get("/jobs/default/web?per_page=25")
	mustContain(t, page, `href="/deployments/d024"`, `rel="next"`)
	mustNotContain(t, page, `href="/deployments/d025"`)
	page = ts.get(link(t, page, "next"))
	mustContain(t, page, `href="/deployments/d029"`, `rel="prev"`)
}
