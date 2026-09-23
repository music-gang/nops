// Package nomadx wraps the parts of the Nomad API that nops uses.
//
// It exists to make the invariants of nops hard to break: Register always
// enforces the job modify index (CAS), and Nomad's error responses, which are
// mostly plain HTTP 500s, are turned into sentinel errors the caller can test
// with errors.Is.
package nomadx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/hashicorp/nomad/api"
)

var (
	// ErrJobNotFound is returned when the job does not exist.
	ErrJobNotFound = errors.New("job not found")
	// ErrCASConflict is returned by RegisterCAS when the job's modify index is
	// not the expected one (or the job exists/does not exist against
	// expectations). The caller must re-detect the current state.
	ErrCASConflict = errors.New("job modify index conflict")
)

// casConflictMarker is the prefix Nomad puts on every enforce-index failure.
// Verified on Nomad 2.0.3, all three variants answer HTTP 500:
//
//	Enforcing job modify index 0: job already exists
//	Enforcing job modify index 999: job exists with conflicting job modify index: 11
//	Enforcing job modify index 5: job does not exist
const casConflictMarker = "Enforcing job modify index"

// Client is a Nomad client bound to one namespace.
type Client struct {
	jobs      *api.Jobs
	namespace string
}

// New creates a Client. Use api.DefaultConfig() to start from the standard
// NOMAD_* environment variables. An empty namespace means "default".
func New(cfg *api.Config, namespace string) (*Client, error) {
	c, err := api.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("create nomad client: %w", err)
	}
	if namespace == "" {
		namespace = api.DefaultNamespace
	}
	return &Client{jobs: c.Jobs(), namespace: namespace}, nil
}

// Namespace returns the namespace the client operates in.
func (c *Client) Namespace() string { return c.namespace }

func (c *Client) query(ctx context.Context) *api.QueryOptions {
	return (&api.QueryOptions{Namespace: c.namespace}).WithContext(ctx)
}

func (c *Client) write(ctx context.Context) *api.WriteOptions {
	return (&api.WriteOptions{Namespace: c.namespace}).WithContext(ctx)
}

// ParseHCL asks Nomad to parse and canonicalize a job. vars is the content of
// an HCL2 var-file and may be empty. Nomad is the only HCL interpreter: there
// is no local parser.
//
// The Nomad API client does not take a context for this call; only an
// already-cancelled context is honoured.
func (c *Client) ParseHCL(ctx context.Context, hcl, vars string) (*api.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	job, err := c.jobs.ParseHCLOpts(&api.JobsParseRequest{
		JobHCL:       hcl,
		Variables:    vars,
		Canonicalize: true,
	})
	if err != nil {
		return nil, fmt.Errorf("parse job: %w", err)
	}
	return job, nil
}

// Job returns the live job, or ErrJobNotFound.
func (c *Client) Job(ctx context.Context, id string) (*api.Job, error) {
	job, _, err := c.jobs.Info(id, c.query(ctx))
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("job %s: %w", id, ErrJobNotFound)
		}
		return nil, fmt.Errorf("get job %s: %w", id, err)
	}
	return job, nil
}

// Plan runs a dry-run registration with the diff included.
func (c *Client) Plan(ctx context.Context, job *api.Job) (*api.JobPlanResponse, error) {
	resp, _, err := c.jobs.PlanOpts(job, &api.PlanOptions{Diff: true}, c.write(ctx))
	if err != nil {
		return nil, fmt.Errorf("plan job %s: %w", deref(job.ID), err)
	}
	return resp, nil
}

// RegisterResult is the outcome of a successful register.
type RegisterResult struct {
	EvalID string
	// JobModifyIndex is what Nomad returned in the register response. Do not
	// treat it as the live job's index: registering a spec identical to the
	// live one leaves the live index unchanged, yet Nomad 2.0.3 can answer with
	// a newer number (seen over the raw HTTP API: response 62, live 61).
	// Re-read the job with Job() when the live index matters.
	JobModifyIndex uint64
}

// RegisterCAS registers job only if the live job's modify index equals
// modifyIndex (0 means "the job must not exist"). There is deliberately no
// variant without the check. preserveCounts keeps group counts owned by an
// autoscaler. A failed check returns an error wrapping ErrCASConflict.
func (c *Client) RegisterCAS(ctx context.Context, job *api.Job, modifyIndex uint64, preserveCounts bool) (*RegisterResult, error) {
	resp, _, err := c.jobs.RegisterOpts(job, &api.RegisterOptions{
		EnforceIndex:   true,
		ModifyIndex:    modifyIndex,
		PreserveCounts: preserveCounts,
	}, c.write(ctx))
	if err != nil {
		if isCASConflict(err) {
			return nil, fmt.Errorf("register job %s at index %d: %w: %v", deref(job.ID), modifyIndex, ErrCASConflict, err)
		}
		return nil, fmt.Errorf("register job %s: %w", deref(job.ID), err)
	}
	return &RegisterResult{EvalID: resp.EvalID, JobModifyIndex: resp.JobModifyIndex}, nil
}

// DispatchResult is the outcome of a dispatch.
type DispatchResult struct {
	// JobID is the ID of the dispatched child job.
	JobID  string
	EvalID string
}

// Dispatch dispatches a parameterized job. A non-empty idempotencyToken makes
// Nomad return the existing child, without a new evaluation, if a child with
// the same token already exists (verified on Nomad 2.0.3, also after the child
// has finished). Nomad rejects meta keys the job does not declare.
func (c *Client) Dispatch(ctx context.Context, parentID string, meta map[string]string, idempotencyToken string) (*DispatchResult, error) {
	wq := c.write(ctx)
	wq.IdempotencyToken = idempotencyToken
	resp, _, err := c.jobs.DispatchOpts(&api.DispatchOptions{JobID: parentID, Meta: meta}, wq)
	if err != nil {
		return nil, fmt.Errorf("dispatch job %s: %w", parentID, err)
	}
	return &DispatchResult{JobID: resp.DispatchedJobID, EvalID: resp.EvalID}, nil
}

func isCASConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), casConflictMarker)
}

func isNotFound(err error) bool {
	var ue api.UnexpectedResponseError
	return errors.As(err, &ue) && ue.StatusCode() == http.StatusNotFound
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
