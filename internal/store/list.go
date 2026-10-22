package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/music-gang/nops/internal/acl"
)

// MinSearch is the shortest search ListDeployments takes: the index finds
// parts of three characters or more.
const MinSearch = 3

// activeStates are the states that hold the per-job lock, as SQL.
const activeStates = `('detected','pending_approval','pre_hook','applying','post_hook')`

// Position is a row of a list of deployments, where a page starts after it.
type Position struct {
	CreatedAt time.Time
	ID        string
}

// PositionOf is the position of d.
func PositionOf(d *Deployment) Position { return Position{CreatedAt: d.CreatedAt, ID: d.ID} }

// DeploymentQuery is a page of the deployments (docs/api.md#lists), newest
// first. A zero field filters nothing, but for Read.
type DeploymentQuery struct {
	// Read is where the reader may read: a page holds only those rows, and is
	// never short of the others. The zero Scope reads nothing.
	Read acl.Scope
	// Search is a part of the job, the namespace, the commit subject or its
	// author, of MinSearch characters or more, or the start of the ID or the
	// commit SHA.
	Search string
	// Active keeps only the deployments that are not in a terminal state;
	// States keeps only these.
	Active                                  bool
	States                                  []State
	Namespace, JobID, Repository, DecidedBy string
	Policy                                  Policy
	// Since and Until keep the deployments created at or after Since, and
	// before Until.
	Since, Until time.Time

	// After starts the page after this row; with Newer, the page is the rows
	// before it, newest first still.
	After *Position
	Newer bool
	Limit int
}

// ListDeployments returns a page of q and whether more rows follow it, past
// its last row (or, with Newer, before its first). The page walks the index of
// its filter until it holds Limit rows: its cost grows with the rows the
// filters skip, never with the rows past the page.
func (s *Store) ListDeployments(ctx context.Context, q DeploymentQuery) ([]*Deployment, bool, error) {
	if q.Limit <= 0 {
		return nil, false, fmt.Errorf("list deployments: limit %d", q.Limit)
	}
	where, args, err := q.where()
	if err != nil {
		return nil, false, err
	}
	order := "DESC"
	if q.After != nil {
		cmp := "<"
		if q.Newer {
			cmp, order = ">", "ASC"
		}
		where = append(where, "(created_at, id) "+cmp+" (?, ?)")
		args = append(args, q.After.CreatedAt.UTC().Format(time.RFC3339Nano), q.After.ID)
	}
	query := `SELECT ` + deploymentCols + ` FROM deployments WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY created_at ` + order + `, id ` + order + ` LIMIT ?`
	list, err := s.queryDeployments(ctx, query, append(args, q.Limit+1)...)
	if err != nil {
		return nil, false, fmt.Errorf("list deployments: %w", err)
	}
	more := len(list) > q.Limit
	if more {
		list = list[:q.Limit]
	}
	if q.Newer && q.After != nil {
		for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
			list[i], list[j] = list[j], list[i]
		}
	}
	return list, more, nil
}

// CountDeployments counts the rows of q, its position and limit aside, up to
// upTo: it returns upTo+1 when there are more. It walks at most upTo+1 of them.
func (s *Store) CountDeployments(ctx context.Context, q DeploymentQuery, upTo int) (int, error) {
	where, args, err := q.where()
	if err != nil {
		return 0, err
	}
	var n int
	err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM deployments WHERE `+strings.Join(where, " AND ")+` LIMIT ?)`,
		append(args, upTo+1)...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count deployments: %w", err)
	}
	return n, nil
}

// where is the condition of q's filters, search and scope.
func (q DeploymentQuery) where() ([]string, []any, error) {
	where, args := []string{"1"}, []any{}
	add := func(cond string, a ...any) {
		where = append(where, cond)
		args = append(args, a...)
	}
	if !q.Read.All {
		cond, a := scopeSQL(q.Read)
		add(cond, a...)
	}
	if q.Search != "" {
		if len([]rune(q.Search)) < MinSearch {
			return nil, nil, fmt.Errorf("search %q is shorter than %d characters", q.Search, MinSearch)
		}
		add(`(id IN (SELECT id FROM deployments_search WHERE deployments_search MATCH ?)
			OR id >= ? AND id < ? OR commit_sha >= ? AND commit_sha < ?)`,
			`"`+strings.ReplaceAll(q.Search, `"`, `""`)+`"`,
			strings.ToUpper(q.Search), strings.ToUpper(q.Search)+"\U0010FFFF",
			strings.ToLower(q.Search), strings.ToLower(q.Search)+"\U0010FFFF")
	}
	if q.Active {
		add("state IN " + activeStates)
	}
	if len(q.States) > 0 {
		marks := strings.TrimSuffix(strings.Repeat("?,", len(q.States)), ",")
		for _, st := range q.States {
			args = append(args, string(st))
		}
		where = append(where, "state IN ("+marks+")")
	}
	for _, f := range []struct{ col, v string }{
		{"namespace", q.Namespace}, {"job_id", q.JobID}, {"repository", q.Repository}, {"decided_by", q.DecidedBy}, {"policy", string(q.Policy)},
	} {
		if f.v != "" {
			add(f.col+" = ?", f.v)
		}
	}
	if !q.Since.IsZero() {
		add("created_at >= ?", q.Since.UTC().Format(time.RFC3339Nano))
	}
	if !q.Until.IsZero() {
		add("created_at < ?", q.Until.UTC().Format(time.RFC3339Nano))
	}
	return where, args, nil
}

// scopeSQL is the condition a namespace column meets where the scope holds:
// a CASE that tests the levels in order, as acl.Scope.Match does, with GLOB
// for path.Match.
func scopeSQL(sc acl.Scope) (string, []any) {
	if len(sc.Levels) == 0 {
		return "0", nil
	}
	var args []any
	matchAny := func(labels []string) string {
		if len(labels) == 0 {
			return "0"
		}
		parts := make([]string, len(labels))
		for i, l := range labels {
			parts[i] = "namespace GLOB ?"
			args = append(args, l)
		}
		return "(" + strings.Join(parts, " OR ") + ")"
	}
	var b strings.Builder
	b.WriteString("CASE")
	for _, l := range sc.Levels {
		b.WriteString(" WHEN " + matchAny(l.Labels) + " THEN NOT " + matchAny(l.Deny) + " AND " + matchAny(l.Grant))
	}
	b.WriteString(" ELSE 0 END")
	return b.String(), args
}
