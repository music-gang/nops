package store

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/acl"
)

var everything = acl.Management().Scope(acl.Read)

func ids(list []*Deployment) []string {
	out := make([]string, len(list))
	for i, d := range list {
		out[i] = d.ID
	}
	return out
}

func mustList(t *testing.T, s *Store, q DeploymentQuery) ([]*Deployment, bool) {
	t.Helper()
	if q.Limit == 0 {
		q.Limit = 100
	}
	if q.Read.Levels == nil && !q.Read.All {
		q.Read = everything
	}
	list, more, err := s.ListDeployments(context.Background(), q)
	if err != nil {
		t.Fatalf("ListDeployments(%+v): %v", q, err)
	}
	return list, more
}

// Walking the pages, older then newer, gives every row once, newest first,
// and says when the list ends, whatever the size of the last page.
func TestListDeploymentsPages(t *testing.T) {
	s := newTestStore(t)
	var want []string
	for i := range 7 {
		want = append([]string{mustCreate(t, s, newDep(fmt.Sprintf("job%d", i))).ID}, want...)
	}

	var got [][]*Deployment
	q := DeploymentQuery{Read: everything, Limit: 3}
	for {
		page, more := mustList(t, s, q)
		got = append(got, page)
		if !more {
			break
		}
		last := PositionOf(page[len(page)-1])
		q.After = &last
	}
	var all []string
	for _, p := range got {
		all = append(all, ids(p)...)
	}
	if !slices.Equal(all, want) || len(got) != 3 || len(got[2]) != 1 {
		t.Fatalf("pages = %v, want %v in pages of 3, 3, 1", all, want)
	}

	// From the last page, newer gives back the page before it.
	first := PositionOf(got[2][0])
	page, more := mustList(t, s, DeploymentQuery{After: &first, Newer: true, Limit: 3})
	if !slices.Equal(ids(page), ids(got[1])) || !more {
		t.Errorf("newer page = %v (more %v), want %v with more before", ids(page), more, ids(got[1]))
	}
	first = PositionOf(got[1][0])
	if page, more := mustList(t, s, DeploymentQuery{After: &first, Newer: true, Limit: 3}); !slices.Equal(ids(page), ids(got[0])) || more {
		t.Errorf("newest page = %v (more %v), want %v and nothing before", ids(page), more, ids(got[0]))
	}
}

func TestListDeploymentsFilters(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	web := mustCreate(t, s, newDep("web"))
	staging := newDep("web")
	staging.Namespace, staging.Repository, staging.Policy = "staging", "infra", PolicyAuto
	staging = mustCreate(t, s, staging)
	db := newDep("db")
	db.State = StatePendingApproval
	db = mustCreate(t, s, db)
	if err := s.Transition(ctx, db.ID, StateRejected, Transition{From: StatePendingApproval, Actor: "alice", DecidedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	api := mustCreate(t, s, newDep("api"))

	for name, tc := range map[string]struct {
		q    DeploymentQuery
		want []*Deployment
	}{
		"active":     {DeploymentQuery{Active: true}, []*Deployment{api, staging, web}},
		"state":      {DeploymentQuery{States: []State{StateRejected}}, []*Deployment{db}},
		"namespace":  {DeploymentQuery{Namespace: "staging"}, []*Deployment{staging}},
		"job":        {DeploymentQuery{JobID: "web"}, []*Deployment{staging, web}},
		"repository": {DeploymentQuery{Repository: "infra"}, []*Deployment{staging}},
		"policy":     {DeploymentQuery{Policy: PolicyApproval}, []*Deployment{api, db, web}},
		"decider":    {DeploymentQuery{DecidedBy: "alice"}, []*Deployment{db}},
		"since":      {DeploymentQuery{Since: db.CreatedAt}, []*Deployment{api, db}},
		"until":      {DeploymentQuery{Until: db.CreatedAt}, []*Deployment{staging, web}},
		"together":   {DeploymentQuery{JobID: "web", Namespace: "default", Active: true}, []*Deployment{web}},
	} {
		t.Run(name, func(t *testing.T) {
			got, _ := mustList(t, s, tc.q)
			if !slices.Equal(ids(got), ids(tc.want)) {
				t.Errorf("got %v, want %v", ids(got), ids(tc.want))
			}
		})
	}
}

func TestListDeploymentsSearch(t *testing.T) {
	s := newTestStore(t)
	fix := newDep("webapp")
	fix.CommitSHA, fix.CommitSubject, fix.CommitAuthor = "f00dcafe", "fix(web): Bump the image", "Iacopo Melani"
	fix = mustCreate(t, s, fix)
	other := newDep("db")
	other.CommitSHA, other.CommitSubject, other.CommitAuthor = "beef0001", "chore: rotate", "someone else"
	other = mustCreate(t, s, other)

	for q, want := range map[string][]*Deployment{
		"bump the":                {fix},
		"MELANI":                  {fix},
		"bapp":                    {fix},
		"F00DC":                   {fix},
		"beef":                    {other},
		strings.ToLower(other.ID): {other},
		"defaul":                  {other, fix},
		`a "quote`:                nil,
		"nothing like this":       nil,
	} {
		got, _ := mustList(t, s, DeploymentQuery{Search: q})
		if !slices.Equal(ids(got), ids(want)) {
			t.Errorf("search %q = %v, want %v", q, ids(got), ids(want))
		}
	}
	if _, _, err := s.ListDeployments(context.Background(), DeploymentQuery{Read: everything, Search: "ab", Limit: 10}); err == nil {
		t.Error("a search of 2 characters went through")
	}
}

// The scope in SQL reads exactly the namespaces Allow allows, on random ACL
// policies: a page is never short of a row the token may read, and never
// holds one it may not.
func TestListDeploymentsScopeAgreesWithAllow(t *testing.T) {
	s := newTestStore(t)
	var names []string
	for _, a := range []string{"", "a", "b", "-"} {
		for _, b := range []string{"a", "b", "-"} {
			names = append(names, a+b)
		}
	}
	for _, ns := range names {
		d := newDep("web")
		d.Namespace = ns
		mustCreate(t, s, d)
	}

	rnd := rand.New(rand.NewPCG(3, 4))
	bodies := []string{`policy = "read"`, `policy = "write"`, `policy = "deny"`, `capabilities = ["approve"]`}
	for range 300 {
		var policies []*acl.Policy
		for range 1 + rnd.IntN(3) {
			var rules strings.Builder
			seen := map[string]bool{}
			for range rnd.IntN(5) {
				var l strings.Builder
				for range 1 + rnd.IntN(3) {
					l.WriteByte("ab*-"[rnd.IntN(4)])
				}
				if seen[l.String()] {
					continue
				}
				seen[l.String()] = true
				fmt.Fprintf(&rules, "namespace %q { %s }\n", l.String(), bodies[rnd.IntN(len(bodies))])
			}
			p, err := acl.Parse(rules.String())
			if err != nil {
				t.Fatal(err)
			}
			policies = append(policies, p)
		}
		a := acl.New(policies...)
		var want []string
		for _, ns := range names {
			if a.Allow(ns, acl.Read) {
				want = append(want, ns)
			}
		}
		got, _, err := s.ListDeployments(context.Background(), DeploymentQuery{Read: a.Scope(acl.Read), Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		var gotNS []string
		for _, d := range got {
			gotNS = append(gotNS, d.Namespace)
		}
		slices.Sort(want)
		slices.Sort(gotNS)
		if !slices.Equal(gotNS, want) {
			t.Fatalf("scope %+v reads %v, Allow allows %v", a.Scope(acl.Read), gotNS, want)
		}
	}
	if got, _, err := s.ListDeployments(context.Background(), DeploymentQuery{Read: acl.New().Scope(acl.Read), Limit: 100}); err != nil || len(got) != 0 {
		t.Errorf("a token with no ACL policy reads %d deployments, %v", len(got), err)
	}
}

func TestCountDeploymentsStopsAtItsCap(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for i := range 4 {
		mustCreate(t, s, newDep(fmt.Sprintf("job%d", i)))
	}
	for max, want := range map[int]int{10: 4, 4: 4, 2: 3} {
		if n, err := s.CountDeployments(ctx, DeploymentQuery{Read: everything}, max); err != nil || n != want {
			t.Errorf("count up to %d = %d, %v; want %d", max, n, err, want)
		}
	}
}

// A deployment made before the search existed is found by it after the
// upgrade.
func TestSearchFindsDeploymentsFromBeforeIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nops.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for v := 1; v <= 16; v++ {
		names, _ := migrationsFS.ReadDir("migrations")
		body, err := migrationsFS.ReadFile("migrations/" + names[v-1].Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("seed %s: %v", names[v-1].Name(), err)
		}
	}
	for _, q := range []string{
		"PRAGMA user_version = 16",
		`INSERT INTO deployments (id, job_id, namespace, repository, commit_sha, commit_subject, spec_hash, job_spec, policy, state, cas_index, created_at, updated_at)
		 VALUES ('old', 'web', 'default', 'default', 'abc1234', 'move the cache', 'h', '{}', 'auto', 'completed', 7, '2026-09-01T00:00:00Z', '2026-09-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed v16: %v", err)
		}
	}
	db.Close()

	s := openAt(t, path)
	got, _ := mustList(t, s, DeploymentQuery{Search: "the cache"})
	if !slices.Equal(ids(got), []string{"old"}) {
		t.Errorf("search after the upgrade = %v, want [old]", ids(got))
	}
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if got, _ := mustList(t, s, DeploymentQuery{Since: since}); len(got) != 1 {
		t.Errorf("since %s = %v, want the old deployment", since, ids(got))
	}
}
