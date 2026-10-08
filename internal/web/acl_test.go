package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/music-gang/nops/internal/acl"
	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/secret"
	"github.com/music-gang/nops/internal/store"
)

// aclServer is a server with the ACL on. Its bootstrap token makes the ACL
// policies and tokens a test needs, through the API.
type aclServer struct {
	*testServer
	boot string
}

func newACLServer(t *testing.T, st Store, en *fakeEngine) aclServer {
	t.Helper()
	boot := secret.New()
	ts := newTestServerLogin(t, st, en, "", nil, "", nil, func(o *Options) { o.ACL, o.BootstrapToken = true, boot })
	return aclServer{ts, boot}
}

// policy saves an ACL policy with the bootstrap token.
func (s aclServer) policy(name, rules string) {
	s.t.Helper()
	body, _ := json.Marshal(policyBody{Rules: rules})
	if rec := s.api("PUT", "/api/acl/policies/"+name, s.boot, string(body)); rec.Code != http.StatusOK {
		s.t.Fatalf("save ACL policy %s: status %d: %s", name, rec.Code, rec.Body)
	}
}

// token makes a token with the bootstrap token and returns its secret.
func (s aclServer) token(in tokenBody) string {
	s.t.Helper()
	body, _ := json.Marshal(in)
	rec := s.api("POST", "/api/acl/tokens", s.boot, string(body))
	if rec.Code != http.StatusCreated {
		s.t.Fatalf("create token: status %d: %s", rec.Code, rec.Body)
	}
	var out apiNewACLToken
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Secret == "" {
		s.t.Fatalf("create token: body %s: %v", rec.Body, err)
	}
	return out.Secret
}

// clientWith makes a client token carrying one ACL policy of these rules.
func (s aclServer) clientWith(rules string) string {
	s.t.Helper()
	s.policy("p", rules)
	return s.token(tokenBody{Name: "client", Type: store.TokenClient, Policies: []string{"p"}})
}

// stubLogin is a login whose every request already acts as sub: the dashboard
// pages are tested against any ACL without going through a token.
type stubLogin struct{ sub *subject }

func (stubLogin) Register(*http.ServeMux) {}
func (l stubLogin) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), userKey{}, l.sub.actor)
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, subjectKey{}, l.sub)))
	})
}

// dashboardAs is a server whose logged-in subject has these rules (none: a
// management token).
func dashboardAs(t *testing.T, st Store, en *fakeEngine, rules string) *testServer {
	t.Helper()
	sub := &subject{actor: "carol", accessorID: "acc-carol", acl: acl.Management()}
	if rules != "" {
		p, err := acl.Parse(rules)
		if err != nil {
			t.Fatal(err)
		}
		sub.acl = acl.New(p)
	}
	return newTestServerLogin(t, st, en, "", nil, "", stubLogin{sub}, func(o *Options) { o.ACL = true; o.BootstrapToken = "x" })
}

func (ts *testServer) page(method, target string, form url.Values) *httptest.ResponseRecorder {
	ts.t.Helper()
	var body *strings.Reader
	if form != nil {
		body = formBody(form)
	}
	return ts.do(method, target, body)
}

// The invariant: no configuration lets a token without "approve" on the job's
// namespace move a deployment past pending_approval, on the API or on the
// dashboard.
func TestNoACLWithoutApproveOnTheNamespaceApproves(t *testing.T) {
	for name, rules := range map[string]string{
		"read only":                  `namespace "default" { policy = "read" }`,
		"every capability but it":    `namespace "default" { capabilities = ["read", "promote", "retry", "deploy-now", "pause"] }`,
		"write on another namespace": `namespace "prod" { policy = "write" }`,
		"a glob that misses":         `namespace "pr*" { policy = "write" }`,
		"a deny on the namespace":    "namespace \"*\" { policy = \"write\" }\nnamespace \"default\" { policy = \"deny\" }",
		"a deny on a closer glob":    "namespace \"*\" { policy = \"write\" }\nnamespace \"def*\" { policy = \"deny\" }",
		"a more specific read":       "namespace \"*\" { policy = \"write\" }\nnamespace \"default\" { policy = \"read\" }",
		"fetch alone":                `capabilities = ["fetch"]`,
		"nothing":                    ``,
	} {
		t.Run(name, func(t *testing.T) {
			en := &fakeEngine{}
			st := &fakeStore{deployment: sampleDeployment()}

			s := newACLServer(t, st, en)
			var tok string
			if rules == "" {
				tok = s.token(tokenBody{Name: "empty", Type: store.TokenClient})
			} else {
				tok = s.clientWith(rules)
			}
			for _, target := range []string{"/api/deployments/d1/approve", "/api/deployments/d1/reject"} {
				if rec := s.api("POST", target, tok, `{"spec_hash":"spec-hash-1"}`); rec.Code != http.StatusForbidden {
					t.Errorf("API %s: status %d, want 403: %s", target, rec.Code, rec.Body)
				}
			}

			var parsed *acl.Policy
			if rules != "" {
				var err error
				if parsed, err = acl.Parse(rules); err != nil {
					t.Fatal(err)
				}
			}
			sub := &subject{actor: "carol", acl: acl.New()}
			if parsed != nil {
				sub.acl = acl.New(parsed)
			}
			d := newTestServerLogin(t, st, en, "", nil, "", stubLogin{sub}, func(o *Options) { o.ACL, o.BootstrapToken = true, "x" })
			for _, target := range []string{"/deployments/d1/approve", "/deployments/d1/reject"} {
				if rec := d.page("POST", target, url.Values{"spec_hash": {"spec-hash-1"}}); rec.Code != http.StatusForbidden {
					t.Errorf("dashboard %s: status %d, want 403", target, rec.Code)
				}
			}

			if len(en.approveCalls) != 0 || len(en.rejectCalls) != 0 {
				t.Errorf("the engine was called: approve %v, reject %v", en.approveCalls, en.rejectCalls)
			}
		})
	}
}

func TestApproveWithTheCapabilityRecordsTheTokenAsTheActor(t *testing.T) {
	en := &fakeEngine{}
	s := newACLServer(t, &fakeStore{deployment: sampleDeployment()}, en)
	s.policy("approvers", `namespace "default" { capabilities = ["read", "approve"] }`)
	var created apiNewACLToken
	rec := s.api("POST", "/api/acl/tokens", s.boot, `{"name":"ci","policies":["approvers"]}`)
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s %v", rec.Code, rec.Body, err)
	}

	if rec := s.api("POST", "/api/deployments/d1/approve", created.Secret, `{"spec_hash":"spec-hash-1"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("approve: status %d: %s", rec.Code, rec.Body)
	}
	if len(en.approveCalls) != 1 || en.approveCalls[0] != (approveCall{"d1", "spec-hash-1", "ci"}) {
		t.Errorf("approve calls = %+v, want one as the token ci", en.approveCalls)
	}
	if en.accessor != created.AccessorID {
		t.Errorf("the engine ran with accessor %q, want %q: the events record it", en.accessor, created.AccessorID)
	}

	// The bootstrap token approves too, as "bootstrap".
	if rec := s.api("POST", "/api/deployments/d1/approve", s.boot, `{"spec_hash":"spec-hash-1"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("bootstrap approve: status %d: %s", rec.Code, rec.Body)
	}
	if got := en.approveCalls[1]; got.actor != "bootstrap" || en.accessor != "bootstrap" {
		t.Errorf("bootstrap approve = %+v, accessor %q, want actor and accessor bootstrap", got, en.accessor)
	}
}

// Every action of the API needs its own capability on the namespace of the
// job, and the others do not stand in for it.
func TestEveryActionNeedsItsCapability(t *testing.T) {
	en := &fakeEngine{retryNext: "d2", deployNowNext: "d3"}
	st := &fakeStore{deployment: sampleDeployment()}
	s := newACLServer(t, st, en)

	for _, c := range []struct {
		name, target, body string
		need               acl.Capability
		global             bool
	}{
		{"approve", "/api/deployments/d1/approve", `{"spec_hash":"h"}`, acl.Approve, false},
		{"reject", "/api/deployments/d1/reject", ``, acl.Approve, false},
		{"promote", "/api/deployments/d1/promote", ``, acl.Promote, false},
		{"retry", "/api/deployments/d1/retry", ``, acl.Retry, false},
		{"pause", "/api/jobs/default/web/pause", ``, acl.Pause, false},
		{"resume", "/api/jobs/default/web/resume", ``, acl.Pause, false},
		{"deploy now", "/api/jobs/default/web/deploy-now", `{"spec_hash":"h"}`, acl.DeployNow, false},
		{"fetch", "/api/fetch", ``, acl.Fetch, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			rules := `namespace "default" { capabilities = ["` + string(c.need) + `"] }`
			// Everything else the namespace has to give, and the global fetch if it is not the one.
			var rest []string
			for _, o := range []acl.Capability{acl.Read, acl.Approve, acl.Promote, acl.Retry, acl.DeployNow, acl.Pause} {
				if o != c.need {
					rest = append(rest, `"`+string(o)+`"`)
				}
			}
			others := `namespace "default" { capabilities = [` + strings.Join(rest, ", ") + `] }`
			if c.global {
				rules, others = `capabilities = ["fetch"]`, `namespace "*" { policy = "write" }`
			}
			with := s.clientWith(rules)
			if rec := s.api("POST", c.target, with, c.body); rec.Code == http.StatusForbidden {
				t.Errorf("with %s: 403: %s", c.need, rec.Body)
			}
			s.policy("p", others)
			if rec := s.api("POST", c.target, with, c.body); rec.Code != http.StatusForbidden {
				t.Errorf("without %s: status %d, want 403", c.need, rec.Code)
			}
		})
	}
}

func TestReadsAreFilteredByNamespace(t *testing.T) {
	prod := dep("p1", "api", store.StateCompleted, time.Hour)
	prod.Namespace = "prod"
	dev := dep("d9", "web", store.StateCompleted, 2*time.Hour)
	en := &fakeEngine{observations: []engine.Observation{
		{Namespace: "default", JobID: "web", Policy: "approval"},
		{Namespace: "prod", JobID: "api", Policy: "approval"},
	}}
	st := &fakeStore{history: []*store.Deployment{prod, dev}, latest: []*store.Deployment{prod, dev}, deployment: dev}
	s := newACLServer(t, st, en)
	tok := s.clientWith(`namespace "prod" { policy = "read" }`)

	var jobs struct {
		Jobs []apiJob `json:"jobs"`
	}
	rec := s.api("GET", "/api/jobs", tok, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &jobs); err != nil || len(jobs.Jobs) != 1 || jobs.Jobs[0].Namespace != "prod" {
		t.Errorf("/api/jobs = %s, want only the job in prod", rec.Body)
	}
	var deps struct {
		Deployments []apiDeployment `json:"deployments"`
	}
	rec = s.api("GET", "/api/deployments", tok, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &deps); err != nil || len(deps.Deployments) != 1 || deps.Deployments[0].Namespace != "prod" {
		t.Errorf("/api/deployments = %s, want only the deployment in prod", rec.Body)
	}
	if rec := s.api("GET", "/api/jobs/default/web", tok, ""); rec.Code != http.StatusForbidden {
		t.Errorf("a job in default: status %d, want 403", rec.Code)
	}
	if rec := s.api("GET", "/api/deployments/d9", tok, ""); rec.Code != http.StatusForbidden {
		t.Errorf("a deployment in default: status %d, want 403", rec.Code)
	}

	// The dashboard filters the same way.
	p, _ := acl.Parse(`namespace "prod" { policy = "read" }`)
	ts := newTestServerLogin(t, st, en, "", nil, "", stubLogin{&subject{actor: "carol", acl: acl.New(p)}}, func(o *Options) { o.ACL, o.BootstrapToken = true, "x" })
	for _, page := range []string{"/jobs", "/history"} {
		body := ts.get(page)
		mustContain(t, body, "prod/api")
		mustNotContain(t, body, "default/web")
	}
	if rec := ts.page("GET", "/jobs/default/web", nil); rec.Code != http.StatusForbidden {
		t.Errorf("the page of a job in default: status %d, want 403", rec.Code)
	}
	if rec := ts.page("GET", "/deployments/d9", nil); rec.Code != http.StatusForbidden {
		t.Errorf("the page of a deployment in default: status %d, want 403", rec.Code)
	}
}

// The dashboard hides what the token can not do.
func TestTheDashboardHidesWhatTheTokenCannotDo(t *testing.T) {
	pending := sampleDeployment()
	failed := dep("d2", "api", store.StateFailed, time.Hour)
	en := &fakeEngine{
		retryable: map[string]bool{"d2": true},
		observations: []engine.Observation{{
			Namespace: "default", JobID: "web", Policy: "approval",
			Hold: &engine.Hold{Kind: engine.HoldPaused, Reason: "db incident", By: "bob", Since: testNow.Add(-time.Hour)},
		}},
	}
	st := &fakeStore{deployment: pending, active: []*store.Deployment{pending}, byJob: []*store.Deployment{failed}, history: []*store.Deployment{failed}, latest: []*store.Deployment{failed}}

	read := dashboardAs(t, st, en, `namespace "*" { policy = "read" }`)
	mustContain(t, read.get("/deployments/d1"), "Review")
	mustNotContain(t, read.get("/deployments/d1"), ">Approve<", ">Reject<")
	job := read.get("/jobs/default/web")
	mustContain(t, job, "Paused by")
	mustNotContain(t, job, ">Resume<", ">Pause<")
	mustNotContain(t, read.get("/history"), ">Retry<")
	mustNotContain(t, read.get("/"), "Fetch now")

	write := dashboardAs(t, st, en, `namespace "*" { policy = "write" }`+"\n"+`capabilities = ["fetch"]`)
	mustContain(t, write.get("/deployments/d1"), ">Approve<", ">Reject<")
	mustContain(t, write.get("/jobs/default/web"), ">Resume<")
	mustContain(t, write.get("/history"), ">Retry<")
	mustContain(t, write.get("/"), "Fetch now")
}

func TestOnlyTheTokensOfTheModeWork(t *testing.T) {
	// A token made with the ACL off stops working when it is turned on, and the
	// other way round.
	off := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	offSecret, _ := makeToken(t, off, "script", time.Time{})
	if rec := off.api("GET", "/api/acl/token/self", offSecret, ""); rec.Code != http.StatusOK {
		t.Fatalf("with the ACL off: status %d: %s", rec.Code, rec.Body)
	}

	on := newACLServer(t, &fakeStore{}, &fakeEngine{})
	if _, err := on.tokens.CreateACLToken(t.Context(), store.ACLToken{Name: "old", Type: store.TokenManagement, ACL: false}, hashToken("nops_old"), store.Audit{Actor: "x"}); err != nil {
		t.Fatal(err)
	}
	if rec := on.api("GET", "/api/jobs", "nops_old", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("a token made with the ACL off, ACL on: status %d, want 401", rec.Code)
	}
}

func TestBootstrapTokenIsAManagementToken(t *testing.T) {
	s := newACLServer(t, &fakeStore{}, &fakeEngine{})
	rec := s.api("GET", "/api/acl/token/self", s.boot, "")
	var self apiACLToken
	if err := json.Unmarshal(rec.Body.Bytes(), &self); err != nil || rec.Code != http.StatusOK ||
		self.AccessorID != "bootstrap" || self.Type != store.TokenManagement {
		t.Errorf("self = %d %s", rec.Code, rec.Body)
	}
	for _, other := range []string{secret.New(), s.boot + "x", s.boot[:len(s.boot)-1]} {
		if rec := s.api("GET", "/api/jobs", other, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("a secret that is not the bootstrap token: status %d, want 401", rec.Code)
		}
	}
}

// With the ACL off the bootstrap token is not configured: nothing is special.
func TestTheBootstrapSecretIsNothingWithTheACLOff(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	if rec := ts.api("GET", "/api/jobs", secret.New(), ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", rec.Code)
	}
}

func TestOnlyAManagementTokenAdministers(t *testing.T) {
	s := newACLServer(t, &fakeStore{}, &fakeEngine{})
	client := s.clientWith(`namespace "*" { policy = "write" }`)

	for _, c := range []struct{ method, target, body string }{
		{"GET", "/api/acl/tokens", ""},
		{"POST", "/api/acl/tokens", `{"name":"x"}`},
		{"GET", "/api/acl/tokens/abc", ""},
		{"DELETE", "/api/acl/tokens/abc", ""},
		{"PUT", "/api/acl/policies/x", `{"rules":"namespace \"*\" { policy = \"read\" }"}`},
		{"DELETE", "/api/acl/policies/p", ""},
		{"GET", "/api/acl/changes", ""},
	} {
		if rec := s.api(c.method, c.target, client, c.body); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with a client token: status %d, want 403", c.method, c.target, rec.Code)
		}
	}
	// A client token reads itself and the ACL policies it carries, and no other.
	s.policy("other", `namespace "*" { policy = "read" }`)
	var self apiACLToken
	rec := s.api("GET", "/api/acl/token/self", client, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &self); err != nil || self.Type != store.TokenClient || len(self.Policies) != 1 || self.Policies[0] != "p" {
		t.Errorf("self = %d %s", rec.Code, rec.Body)
	}
	if rec := s.api("GET", "/api/acl/policies/p", client, ""); rec.Code != http.StatusOK {
		t.Errorf("a policy it carries: status %d", rec.Code)
	}
	if rec := s.api("GET", "/api/acl/policies/other", client, ""); rec.Code != http.StatusForbidden {
		t.Errorf("a policy it does not carry: status %d, want 403", rec.Code)
	}
	var list struct {
		Policies []apiACLPolicy `json:"acl_policies"`
	}
	rec = s.api("GET", "/api/acl/policies", client, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Policies) != 1 || list.Policies[0].Name != "p" {
		t.Errorf("the policies of a client token = %s, want only p", rec.Body)
	}
}

func TestEditingAnACLPolicyChangesEveryTokenThatCarriesIt(t *testing.T) {
	en := &fakeEngine{}
	s := newACLServer(t, &fakeStore{deployment: sampleDeployment()}, en)
	tok := s.clientWith(`namespace "default" { capabilities = ["read"] }`)
	approve := func() int {
		return s.api("POST", "/api/deployments/d1/approve", tok, `{"spec_hash":"h"}`).Code
	}
	if code := approve(); code != http.StatusForbidden {
		t.Fatalf("before the edit: status %d, want 403", code)
	}
	s.policy("p", `namespace "default" { capabilities = ["read", "approve"] }`)
	if code := approve(); code != http.StatusNoContent {
		t.Errorf("after granting approve: status %d, want 204", code)
	}
	// A deleted ACL policy grants nothing, and the token stays valid.
	if rec := s.api("DELETE", "/api/acl/policies/p", s.boot, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d", rec.Code)
	}
	if code := approve(); code != http.StatusForbidden {
		t.Errorf("after deleting the ACL policy: status %d, want 403", code)
	}
	if rec := s.api("GET", "/api/acl/token/self", tok, ""); rec.Code != http.StatusOK {
		t.Errorf("the token itself: status %d, want 200", rec.Code)
	}
}

func TestInvalidRulesSaveNothing(t *testing.T) {
	s := newACLServer(t, &fakeStore{}, &fakeEngine{})
	for name, rules := range map[string]string{
		"not HCL":            `namespace {{`,
		"an unknown policy":  `namespace "a" { policy = "admin" }`,
		"a bad capability":   `namespace "a" { capabilities = ["submit"] }`,
		"empty":              `   `,
		"capability outside": `capabilities = ["approve"]`,
	} {
		body, _ := json.Marshal(policyBody{Rules: rules})
		rec := s.api("PUT", "/api/acl/policies/bad", s.boot, string(body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
			continue
		}
		apiErrorOf(t, rec)
	}
	if rec := s.api("GET", "/api/acl/policies/bad", s.boot, ""); rec.Code != http.StatusNotFound {
		t.Errorf("after the refused rules: status %d, want 404: nothing is saved", rec.Code)
	}
	if rec := s.api("PUT", "/api/acl/policies/a%20b", s.boot, `{"rules":"namespace \"*\" { policy = \"read\" }"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("a bad name: status %d, want 400", rec.Code)
	}
}

func TestCreateTokenRefusesWhatIsWrong(t *testing.T) {
	s := newACLServer(t, &fakeStore{}, &fakeEngine{})
	s.policy("p", `namespace "*" { policy = "read" }`)
	for name, body := range map[string]string{
		"no name":                      `{}`,
		"an unknown type":              `{"name":"x","type":"root"}`,
		"a management token with ACLs": `{"name":"x","type":"management","policies":["p"]}`,
		"an ACL policy that is absent": `{"name":"x","policies":["nope"]}`,
		"a bad duration":               `{"name":"x","expires_in":"soon"}`,
		"a negative duration":          `{"name":"x","expires_in":"-1h"}`,
		"an unknown field":             `{"name":"x","admin":true}`,
	} {
		if rec := s.api("POST", "/api/acl/tokens", s.boot, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", name, rec.Code, rec.Body)
		}
	}
}

func TestTokensExpireAndAreRevoked(t *testing.T) {
	s := newACLServer(t, &fakeStore{}, &fakeEngine{})
	tok := s.token(tokenBody{Name: "short", Type: store.TokenManagement, ExpiresIn: "1h"})
	if rec := s.api("GET", "/api/acl/token/self", tok, ""); rec.Code != http.StatusOK {
		t.Fatalf("before it expires: status %d", rec.Code)
	}
	*s.clock = s.clock.Add(2 * time.Hour)
	if rec := s.api("GET", "/api/acl/token/self", tok, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("after it expires: status %d, want 401", rec.Code)
	}

	keep := s.token(tokenBody{Name: "keep", Type: store.TokenManagement})
	var self apiACLToken
	rec := s.api("GET", "/api/acl/token/self", keep, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &self); err != nil || self.CreatorAccessorID != "bootstrap" || self.CreatorName != "bootstrap" || self.ExpiresAt != nil {
		t.Fatalf("self = %s, want the bootstrap token as its creator and no expiry", rec.Body)
	}
	if rec := s.api("DELETE", "/api/acl/tokens/"+self.AccessorID, s.boot, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: status %d", rec.Code)
	}
	if rec := s.api("GET", "/api/acl/token/self", keep, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("after the revoke: status %d, want 401", rec.Code)
	}
	if rec := s.api("DELETE", "/api/acl/tokens/"+self.AccessorID, s.boot, ""); rec.Code != http.StatusNotFound {
		t.Errorf("revoking twice: status %d, want 404", rec.Code)
	}
}

func TestChangesRecordWhoMadeThem(t *testing.T) {
	s := newACLServer(t, &fakeStore{}, &fakeEngine{})
	s.policy("p", `namespace "*" { policy = "read" }`)
	s.token(tokenBody{Name: "t", Type: store.TokenClient, Policies: []string{"p"}})

	var out struct {
		Changes []apiChange `json:"changes"`
	}
	rec := s.api("GET", "/api/acl/changes", s.boot, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Changes) != 2 {
		t.Fatalf("changes = %s", rec.Body)
	}
	for _, c := range out.Changes {
		if c.AccessorID != "bootstrap" || c.Actor != "bootstrap" {
			t.Errorf("change %+v, want it made by the bootstrap token", c)
		}
	}
	if c := out.Changes[0]; c.Kind != "token" || c.Action != "create" {
		t.Errorf("newest change = %+v, want the token created", c)
	}
}

func TestAdministrationPage(t *testing.T) {
	st := &fakeStore{}
	manager := dashboardAs(t, st, &fakeEngine{}, "")
	// The login of a stub has no row: it lists what the store holds.
	if _, err := manager.tokens.CreateACLToken(t.Context(), store.ACLToken{Name: "ci", Type: store.TokenClient, Policies: []string{"readers"}, ACL: true, CreatorName: "bootstrap"}, "h", store.Audit{Actor: "bootstrap"}); err != nil {
		t.Fatal(err)
	}
	if err := manager.tokens.PutACLPolicy(t.Context(), store.ACLPolicy{Name: "readers", Description: "see all", Rules: `namespace "*" { policy = "read" }`}, store.Audit{Actor: "bootstrap"}); err != nil {
		t.Fatal(err)
	}
	page := manager.get("/admin")
	mustContain(t, page, "Administration", "Your token", "New token", "ci", "readers", "see all", "Latest changes", "New ACL policy")
	mustNotContain(t, page, "The ACL is off")

	// A client token sees itself and nothing to administer.
	client := dashboardAs(t, st, &fakeEngine{}, `namespace "*" { policy = "read" }`)
	page = client.get("/admin")
	mustContain(t, page, "Your token")
	mustNotContain(t, page, "New token", "New ACL policy", "Latest changes")
	for _, c := range []struct{ method, target string }{
		{"POST", "/admin/tokens"}, {"POST", "/admin/policies"}, {"GET", "/admin/policies/readers"},
		{"POST", "/admin/policies/readers/delete"}, {"POST", "/admin/tokens/abc/revoke"},
	} {
		if rec := client.page(c.method, c.target, url.Values{}); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as a client token: status %d, want 403", c.method, c.target, rec.Code)
		}
	}
}

func TestAdministrationSaysWhenTheACLIsOff(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	page := ts.get("/admin")
	mustContain(t, page, "The ACL is off", "full control", "New token")
}

func TestAdministrationFormsManageTokensAndACLPolicies(t *testing.T) {
	ts := dashboardAs(t, &fakeStore{}, &fakeEngine{}, "")

	// An invalid ACL policy shows the error, keeps what was typed and saves nothing.
	bad := url.Values{"name": {"readers"}, "rules": {`namespace "a" { policy = "admin" }`}}
	rec := ts.page("POST", "/admin/policies", bad)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad rules: status %d, want 400", rec.Code)
	}
	mustContain(t, rec.Body.String(), `&#34;admin&#34;`, "readers")
	if _, err := ts.tokens.ACLPolicy(t.Context(), "readers"); err == nil {
		t.Error("the refused rules were saved")
	}

	good := url.Values{"name": {"readers"}, "description": {"see all"}, "rules": {`namespace "*" { policy = "read" }`}}
	if rec := ts.page("POST", "/admin/policies", good); rec.Code != http.StatusSeeOther {
		t.Fatalf("save: status %d, body %s", rec.Code, rec.Body)
	}
	mustContain(t, ts.get("/admin/policies/readers"), "see all", "policy = &#34;read&#34;")

	rec = ts.page("POST", "/admin/tokens", url.Values{"name": {"ci"}, "type": {"client"}, "policy": {"readers"}, "expires": {"30"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status %d, body %s", rec.Code, rec.Body)
	}
	mustContain(t, rec.Body.String(), "Token “ci” created", "nops_")
	list, err := ts.tokens.ACLTokens(t.Context())
	if err != nil || len(list) != 1 || list[0].Name != "ci" || len(list[0].Policies) != 1 || list[0].ExpiresAt.IsZero() {
		t.Fatalf("tokens = %+v, %v", list, err)
	}

	if rec := ts.page("POST", "/admin/tokens", url.Values{"name": {"x"}, "policy": {"nope"}, "expires": {"30"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("an absent ACL policy: status %d, want 400", rec.Code)
	}
	if rec := ts.page("POST", "/admin/tokens", url.Values{"name": {"x"}, "expires": {"forever"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("a bad expiry: status %d, want 400", rec.Code)
	}

	if rec := ts.page("POST", "/admin/tokens/"+list[0].AccessorID+"/revoke", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Errorf("revoke: status %d", rec.Code)
	}
	if rec := ts.page("POST", "/admin/policies/readers/delete", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Errorf("delete: status %d", rec.Code)
	}
	if rec := ts.page("GET", "/admin/policies/readers", nil); rec.Code != http.StatusNotFound {
		t.Errorf("a deleted ACL policy: status %d, want 404", rec.Code)
	}
}

// With the ACL on a login carries no token yet: it sees the pages and does nothing.
func TestALoginWithTheACLOnCanDoNothing(t *testing.T) {
	en := &fakeEngine{}
	s := newACLServer(t, &fakeStore{deployment: sampleDeployment()}, en)
	cookie := mintSession(t, s.auth, "alice")

	if rec := s.do("GET", "/", nil, cookie); rec.Code != http.StatusOK {
		t.Errorf("the Overview: status %d, want 200", rec.Code)
	}
	if rec := s.do("GET", "/deployments/d1", nil, cookie); rec.Code != http.StatusForbidden {
		t.Errorf("a deployment: status %d, want 403", rec.Code)
	}
	if rec := s.do("POST", "/deployments/d1/approve", formBody(url.Values{"spec_hash": {"spec-hash-1"}}), cookie); rec.Code != http.StatusForbidden {
		t.Errorf("approve: status %d, want 403", rec.Code)
	}
	if len(en.approveCalls) != 0 {
		t.Errorf("approved: %v", en.approveCalls)
	}
}

func TestTokenSecretsAreNotKept(t *testing.T) {
	s := newACLServer(t, &fakeStore{}, &fakeEngine{})
	sec := s.token(tokenBody{Name: "t", Type: store.TokenManagement})
	if _, err := s.tokens.ACLTokenBySecret(t.Context(), sec, true); err == nil {
		t.Error("the store knows the token by its secret: it should know only the hash")
	}
	if _, err := s.tokens.ACLTokenBySecret(t.Context(), hashToken(sec), true); err != nil {
		t.Errorf("the store does not know the token by the hash of its secret: %v", err)
	}
}
