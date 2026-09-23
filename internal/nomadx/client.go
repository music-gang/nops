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
	"sort"
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

// New creates a Client. nops passes config.Config.Nomad(), which ignores the
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

// Alloc is the part of an allocation that hook outcome detection needs.
type Alloc struct {
	ID string
	// ClientStatus is pending, running, complete, failed or lost.
	ClientStatus string
	// DesiredStatus is what the server wants: run, stop or evict. A batch
	// allocation that completed on its own keeps "run".
	DesiredStatus string
	// Failure explains a failed allocation (the client description plus, for each
	// failed task, the event that says why). Empty when nothing failed.
	Failure string
}

// Allocations lists every allocation of a job, including finished ones.
func (c *Client) Allocations(ctx context.Context, jobID string) ([]Alloc, error) {
	stubs, _, err := c.jobs.Allocations(jobID, true, c.query(ctx))
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("allocations of job %s: %w", jobID, ErrJobNotFound)
		}
		return nil, fmt.Errorf("list allocations of job %s: %w", jobID, err)
	}
	out := make([]Alloc, 0, len(stubs))
	for _, s := range stubs {
		out = append(out, Alloc{
			ID:            s.ID,
			ClientStatus:  s.ClientStatus,
			DesiredStatus: s.DesiredStatus,
			Failure:       failure(s),
		})
	}
	return out, nil
}

// failure builds a human-readable reason for a failed allocation.
func failure(s *api.AllocationListStub) string {
	var parts []string
	if s.ClientStatus == "failed" || s.ClientStatus == "lost" {
		if s.ClientDescription != "" {
			parts = append(parts, s.ClientDescription)
		}
	}
	names := make([]string, 0, len(s.TaskStates))
	for name := range s.TaskStates {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ts := s.TaskStates[name]
		if ts == nil || !ts.Failed || len(ts.Events) == 0 {
			continue
		}
		ev := failureEvent(ts.Events)
		msg := ev.DisplayMessage
		if msg == "" {
			msg = ev.Type
		}
		parts = append(parts, fmt.Sprintf("task %s: %s", name, msg))
	}
	return strings.Join(parts, "; ")
}

// failureEvent picks the event that says why a task failed. The last event is
// usually "Not Restarting: Policy allows no restarts", which hides the cause,
// so the last cause-type event wins and the last event is the fallback.
func failureEvent(events []*api.TaskEvent) *api.TaskEvent {
	for i := len(events) - 1; i >= 0; i-- {
		switch events[i].Type {
		case api.TaskTerminated, api.TaskDriverFailure, api.TaskSetupFailure, api.TaskFailedValidation:
			return events[i]
		}
	}
	return events[len(events)-1]
}

// FindDispatched returns the ID of the child of parentID that was dispatched
// with idempotencyToken, or "" if there is none (never dispatched, or already
// garbage-collected). It is how a resumed run finds a child whose ID was not
// saved. It lists the children (dead ones included) and reads each one, as the
// token is only on the full job.
func (c *Client) FindDispatched(ctx context.Context, parentID, idempotencyToken string) (string, error) {
	q := c.query(ctx)
	q.Prefix = parentID + "/dispatch-"
	stubs, _, err := c.jobs.List(q)
	if err != nil {
		return "", fmt.Errorf("list children of job %s: %w", parentID, err)
	}
	for _, s := range stubs {
		if s.ParentID != parentID {
			continue
		}
		child, err := c.Job(ctx, s.ID)
		if errors.Is(err, ErrJobNotFound) {
			continue // garbage-collected between the list and the read
		}
		if err != nil {
			return "", err
		}
		if child.DispatchIdempotencyToken != nil && *child.DispatchIdempotencyToken == idempotencyToken {
			return s.ID, nil
		}
	}
	return "", nil
}

// StopJob deregisters a job without purging it, so it stays visible in Nomad
// for debugging. A job that does not exist is not an error: stopping is
// idempotent.
func (c *Client) StopJob(ctx context.Context, id string) error {
	if _, _, err := c.jobs.Deregister(id, false, c.write(ctx)); err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("stop job %s: %w", id, err)
	}
	return nil
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
