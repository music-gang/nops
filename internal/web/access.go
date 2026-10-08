package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/music-gang/nops/internal/acl"
	"github.com/music-gang/nops/internal/engine"
	"github.com/music-gang/nops/internal/store"
)

// bootstrapAccessorID names the bootstrap token in the audit and the logs.
const bootstrapAccessorID = "bootstrap"

// subject is who a request acts as, and what that allows (docs/acl.md). Every
// request has one: a token on /api/, the session on the dashboard.
type subject struct {
	actor      string // what the records show: a person's name or a token's name
	accessorID string // empty for a dashboard session
	token      *store.ACLToken
	acl        *acl.ACL
}

type subjectKey struct{}

// subjectOf returns the subject of the request. A request that reached a
// handler without one gets a subject that can do nothing.
func subjectOf(ctx context.Context) *subject {
	if sub, ok := ctx.Value(subjectKey{}).(*subject); ok {
		return sub
	}
	return &subject{acl: acl.New()}
}

// allows reports whether the request's subject has the capability on the namespace.
func allows(r *http.Request, namespace string, c acl.Capability) bool {
	return subjectOf(r.Context()).acl.Allow(namespace, c)
}

// hashToken is what the store keeps of a token: nobody who reads the
// database can use it to call the API.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// sessionSubject is who a dashboard session acts as. With the ACL off a login
// can do everything, as before the ACL. With it on, a login carries no token
// yet and can do nothing.
func (s *server) sessionSubject(user string) *subject {
	if !s.aclOn {
		return &subject{actor: user, acl: acl.Management()}
	}
	return &subject{actor: user, acl: acl.New()}
}

// tokenSubject finds who a bearer token acts as. It returns store.ErrNotFound
// for a token that is unknown, expired, revoked or made in the other mode.
func (s *server) tokenSubject(ctx context.Context, secret string) (*subject, error) {
	if s.aclOn && s.bootstrap != "" && equal(secret, s.bootstrap) {
		return &subject{actor: bootstrapAccessorID, accessorID: bootstrapAccessorID, acl: acl.Management()}, nil
	}
	t, err := s.access.ACLTokenBySecret(ctx, hashToken(secret), s.aclOn)
	if err != nil {
		return nil, err
	}
	sub := &subject{actor: t.Name, accessorID: t.AccessorID, token: &t}
	switch {
	case !s.aclOn, t.Type == store.TokenManagement:
		sub.acl = acl.Management()
	default:
		if sub.acl, err = s.clientACL(ctx, t.Policies); err != nil {
			return nil, err
		}
	}
	return sub, nil
}

// clientACL combines the ACL policies a client token carries, by name, as they
// are now: editing one changes every token that carries it. A name that no
// longer exists, or whose rules no longer parse, grants nothing.
func (s *server) clientACL(ctx context.Context, names []string) (*acl.ACL, error) {
	var policies []*acl.Policy
	for _, name := range names {
		p, err := s.access.ACLPolicy(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		parsed, err := acl.Parse(p.Rules)
		if err != nil {
			s.log.ErrorContext(ctx, "an ACL policy no longer parses: it grants nothing", "acl_policy", name, "error", err)
			continue
		}
		policies = append(policies, parsed)
	}
	return acl.New(policies...), nil
}

// A check says whether the subject may do what a route does.
type check func(s *server, r *http.Request, sub *subject) (bool, error)

// anyToken lets every valid token through: what it shows, it filters itself.
func anyToken() check {
	return func(*server, *http.Request, *subject) (bool, error) { return true, nil }
}

// inNamespace needs the capability on the namespace of the route.
func inNamespace(c acl.Capability) check {
	return func(_ *server, r *http.Request, sub *subject) (bool, error) {
		return sub.acl.Allow(r.PathValue("namespace"), c), nil
	}
}

// onDeployment needs the capability on the namespace of the deployment of the
// route. A deployment that does not exist passes, so the handler answers 404.
func onDeployment(c acl.Capability) check {
	return func(s *server, r *http.Request, sub *subject) (bool, error) {
		d, err := s.store.GetDeployment(r.Context(), r.PathValue("id"))
		if errors.Is(err, store.ErrNotFound) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return sub.acl.Allow(d.Namespace, c), nil
	}
}

// globally needs a capability that belongs to no namespace.
func globally(c acl.Capability) check {
	return func(_ *server, _ *http.Request, sub *subject) (bool, error) { return sub.acl.Global(c), nil }
}

// management needs a management token.
func management() check {
	return func(_ *server, _ *http.Request, sub *subject) (bool, error) { return sub.acl.Management(), nil }
}

// guard is the one check of the dashboard and the API: it resolves the subject
// of the request, asks the route's check, and answers 403 through denied if
// the subject may not. The handlers behind it see the subject in the context,
// and the events they cause carry its accessor ID.
func (s *server) guard(c check, denied func(http.ResponseWriter, *http.Request), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub, ok := r.Context().Value(subjectKey{}).(*subject)
		if !ok { // a dashboard session: the token of an API request is resolved before
			user, _ := UserFrom(r.Context())
			sub = s.sessionSubject(user)
		}
		allowed, err := c(s, r, sub)
		if err != nil {
			s.serverError(w, r, "check the request", err)
			return
		}
		if !allowed {
			s.log.WarnContext(r.Context(), "request refused by the ACL", "method", r.Method, "path", r.URL.Path, "actor", sub.actor, "accessor_id", sub.accessorID)
			denied(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), subjectKey{}, sub)
		next.ServeHTTP(w, r.WithContext(store.WithAccessor(ctx, sub.accessorID)))
	})
}

// page guards a dashboard route: a login, then the check.
func (s *server) page(c check, h http.HandlerFunc) http.Handler {
	return s.auth.Require(s.guard(c, s.forbidden, h))
}

// forbidden renders the 403 page.
func (s *server) forbidden(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusForbidden)
	s.render(w, r, "error", errorData{
		baseData: s.base(r, ""),
		Status:   http.StatusForbidden,
		Title:    "Not allowed",
		Message:  "Your token does not allow this.",
	})
}

// readable keeps the deployments in namespaces the subject can read.
func readable(r *http.Request, list []*store.Deployment) []*store.Deployment {
	var out []*store.Deployment
	for _, d := range list {
		if allows(r, d.Namespace, acl.Read) {
			out = append(out, d)
		}
	}
	return out
}

// observations are the jobs the engine watches in namespaces the subject can read.
func (s *server) observations(r *http.Request) []engine.Observation {
	var out []engine.Observation
	for _, o := range s.engine.Observations() {
		if allows(r, o.Namespace, acl.Read) {
			out = append(out, o)
		}
	}
	return out
}

// orphans are the orphan jobs in namespaces the subject can read.
func (s *server) orphans(r *http.Request) []engine.Orphan {
	var out []engine.Orphan
	for _, o := range s.engine.Orphans() {
		if allows(r, o.Namespace, acl.Read) {
			out = append(out, o)
		}
	}
	return out
}
