package web

import (
	"strings"
	"testing"

	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/store"
)

// newLinkedServer is a test server that reads a Nomad for its panel and knows
// where people open the Nomad UI.
func newLinkedServer(t *testing.T, st Store, nomadUI string, nom Nomad) *testServer {
	t.Helper()
	return newTestServerOptions(t, st, &fakeEngine{}, "", nom, nomadUI)
}

func TestNomadLinkURLs(t *testing.T) {
	for name, tt := range map[string]struct {
		base, ns, job, jobURL string
	}{
		"plain":            {"https://nomad.example.com", "default", "web", "https://nomad.example.com/ui/jobs/web@default"},
		"trailing slash":   {"https://nomad.example.com/", "default", "web", "https://nomad.example.com/ui/jobs/web@default"},
		"sub path":         {"https://infra.example.com/nomad", "default", "web", "https://infra.example.com/nomad/ui/jobs/web@default"},
		"other namespace":  {"https://nomad.example.com", "apps", "api", "https://nomad.example.com/ui/jobs/api@apps"},
		"dispatched child": {"https://nomad.example.com", "default", "hook/dispatch-1-ab", "https://nomad.example.com/ui/jobs/hook%2Fdispatch-1-ab@default"},
	} {
		t.Run(name, func(t *testing.T) {
			s := &server{nomadUI: strings.TrimRight(tt.base, "/")}
			if got := s.nomadJobURL(tt.ns, tt.job); got != tt.jobURL {
				t.Errorf("job URL = %q, want %q", got, tt.jobURL)
			}
			if got := s.nomadDeploymentsURL(tt.ns, tt.job); got != tt.jobURL+"/deployments" {
				t.Errorf("deployments URL = %q, want %q", got, tt.jobURL+"/deployments")
			}
		})
	}

	// No -nomad-ui-url, no link: never one built from anything else.
	s := &server{}
	if s.nomadJobURL("default", "web") != "" || s.nomadDeploymentsURL("default", "web") != "" {
		t.Error("a link was built without a Nomad UI address")
	}
}

const openInNomad = "Open in Nomad"

func TestJobPageLinksToTheJobInNomad(t *testing.T) {
	st := &fakeStore{byJob: []*store.Deployment{applyingDeployment()}}
	page := newLinkedServer(t, st, "https://infra.example.com/nomad", canaryNomad()).get("/jobs/default/web")
	mustContain(t, page, `href="https://infra.example.com/nomad/ui/jobs/web@default"`, openInNomad, `rel="noopener noreferrer"`,
		// the panel's Nomad deployment goes to the job's deployments tab
		`href="https://infra.example.com/nomad/ui/jobs/web@default/deployments"`)

	// Whatever the job's state in git: an orphan still runs in Nomad.
	en := &fakeEngine{orphans: []engine.Orphan{{JobID: "web", Namespace: "default", NomadStatus: "running"}}}
	ts := newTestServerOptions(t, &fakeStore{}, en, "", canaryNomad(), "https://nomad.example.com")
	mustContain(t, ts.get("/jobs/default/web"), `href="https://nomad.example.com/ui/jobs/web@default"`)
}

func TestDeploymentPageLinksToNomad(t *testing.T) {
	st := &fakeStore{deployment: applyingDeployment()}
	ts := newLinkedServer(t, st, "https://nomad.example.com", canaryNomad())
	const job, tab = `href="https://nomad.example.com/ui/jobs/web@default"`, `href="https://nomad.example.com/ui/jobs/web@default/deployments"`

	// The page and the fragment it polls: the links survive the swap.
	for _, path := range []string{"/deployments/d1", "/deployments/d1/status"} {
		page := ts.get(path)
		mustContain(t, page, job, tab)
		// the job in the head, and the deployments tab in the notice and in the panel
		if n := strings.Count(page, tab); n != 2 {
			t.Errorf("%s: %d links to the deployments tab, want 2 (the wait notice and the panel)", path, n)
		}
		if n := strings.Count(page, openInNomad); n != 2 {
			t.Errorf("%s: %d %q links, want 2 (the head and the wait notice)", path, n, openInNomad)
		}
	}

	// A deployment that is not applying has no panel and no notice, and still links to its job.
	done := applyingDeployment()
	done.State = store.StateCompleted
	page := newLinkedServer(t, &fakeStore{deployment: done}, "https://nomad.example.com", canaryNomad()).get("/deployments/d1")
	mustContain(t, page, job)
	mustNotContain(t, page, tab)
}

func TestNoLinksWithoutANomadUIAddress(t *testing.T) {
	st := &fakeStore{deployment: applyingDeployment(), byJob: []*store.Deployment{applyingDeployment()}}
	ts := newLinkedServer(t, st, "", canaryNomad())
	for _, path := range []string{"/jobs/default/web", "/deployments/d1", "/deployments/d1/status"} {
		page := ts.get(path)
		mustNotContain(t, page, openInNomad, "/ui/jobs/")
		if !strings.Contains(page, "Nomad") { // the pages themselves are still there
			t.Errorf("%s: no Nomad panel either", path)
		}
	}
}
