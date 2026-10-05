package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"
)

// What the API answers, as far as the text output reads it (docs/api.md).

type deployment struct {
	ID            string    `json:"id"`
	Namespace     string    `json:"namespace"`
	Job           string    `json:"job"`
	State         string    `json:"state"`
	Policy        string    `json:"policy"`
	SpecHash      string    `json:"spec_hash"`
	CommitSHA     string    `json:"commit_sha"`
	CommitSubject string    `json:"commit_subject"`
	CommitAuthor  string    `json:"commit_author"`
	Error         string    `json:"error"`
	DecidedBy     string    `json:"decided_by"`
	RetryOf       string    `json:"retry_of"`
	CreatedAt     time.Time `json:"created_at"`
}

type deploymentDetail struct {
	deployment
	Diff   json.RawMessage `json:"diff"`
	Events []struct {
		Time    time.Time `json:"time"`
		From    string    `json:"from"`
		To      string    `json:"to"`
		Actor   string    `json:"actor"`
		Message string    `json:"message"`
	} `json:"events"`
	HookRuns []struct {
		Phase string `json:"phase"`
		Job   string `json:"job"`
		State string `json:"state"`
		Error string `json:"error"`
	} `json:"hook_runs"`
}

type job struct {
	Namespace string `json:"namespace"`
	Job       string `json:"job"`
	Policy    string `json:"policy"`
	Sync      string `json:"sync"`
	SpecHash  string `json:"spec_hash"`
	BlockedBy string `json:"blocked_by"`
	Hold      *struct {
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
		By     string `json:"by"`
	} `json:"hold"`
	LastDeployment *deployment `json:"last_deployment"`
}

type jobDetail struct {
	job
	File   string `json:"file"`
	Issues []struct {
		Severity string `json:"severity"`
		Key      string `json:"key"`
		Message  string `json:"message"`
	} `json:"issues"`
	Diff        json.RawMessage `json:"diff"`
	Deployments []deployment    `json:"deployments"`
}

func table(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0) }

func stamp(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// dash stands for an empty value in a table, so the columns keep their place.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func writeJobs(w io.Writer, jobs []job) {
	if len(jobs) == 0 {
		fmt.Fprintln(w, "No jobs.")
		return
	}
	t := table(w)
	fmt.Fprintln(t, "NAMESPACE\tJOB\tPOLICY\tSYNC\tLAST DEPLOYMENT")
	for _, j := range jobs {
		last := "-"
		if d := j.LastDeployment; d != nil {
			last = d.ID + " " + d.State
		}
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\n", j.Namespace, j.Job, dash(j.Policy), j.Sync, last)
	}
	t.Flush()
}

func writeDeployments(w io.Writer, list []deployment) {
	if len(list) == 0 {
		fmt.Fprintln(w, "No deployments.")
		return
	}
	t := table(w)
	fmt.Fprintln(t, "ID\tNAMESPACE\tJOB\tSTATE\tPOLICY\tCOMMIT\tCREATED")
	for _, d := range list {
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", d.ID, d.Namespace, d.Job, d.State, d.Policy, dash(shortSHA(d.CommitSHA)), stamp(d.CreatedAt))
	}
	t.Flush()
}

// field prints a "name: value" line of a detail, unless there is no value.
func field(w io.Writer, name, v string) {
	if v != "" {
		fmt.Fprintf(w, "%-12s %s\n", name+":", v)
	}
}

func writeDeployment(w io.Writer, d deploymentDetail) error {
	field(w, "Deployment", d.ID)
	field(w, "Job", d.Namespace+"/"+d.Job)
	field(w, "State", d.State)
	field(w, "Policy", d.Policy)
	field(w, "Spec hash", d.SpecHash)
	if d.CommitSHA != "" {
		field(w, "Commit", shortSHA(d.CommitSHA)+" "+d.CommitSubject)
	}
	field(w, "Author", d.CommitAuthor)
	field(w, "Decided by", d.DecidedBy)
	field(w, "Retry of", d.RetryOf)
	field(w, "Error", d.Error)
	field(w, "Created", stamp(d.CreatedAt))
	fmt.Fprintln(w)
	if err := writeDiff(w, d.Diff); err != nil {
		return err
	}
	if len(d.HookRuns) > 0 {
		fmt.Fprintln(w, "\nHook runs")
		t := table(w)
		for _, h := range d.HookRuns {
			fmt.Fprintf(t, "  %s\t%s\t%s\t%s\n", h.Phase, h.Job, h.State, h.Error)
		}
		t.Flush()
	}
	if len(d.Events) > 0 {
		fmt.Fprintln(w, "\nEvents")
		t := table(w)
		for _, e := range d.Events {
			fmt.Fprintf(t, "  %s\t%s -> %s\t%s\t%s\n", stamp(e.Time), e.From, e.To, e.Actor, e.Message)
		}
		t.Flush()
	}
	return nil
}

func writeJob(w io.Writer, j jobDetail) error {
	field(w, "Job", j.Namespace+"/"+j.Job)
	field(w, "Policy", j.Policy)
	field(w, "Sync", j.Sync)
	field(w, "Spec hash", j.SpecHash)
	field(w, "File", j.File)
	field(w, "Blocked by", j.BlockedBy)
	if h := j.Hold; h != nil {
		field(w, "Held", h.Kind+": "+h.Reason)
	}
	if len(j.Issues) > 0 {
		fmt.Fprintln(w, "\nIssues")
		for _, i := range j.Issues {
			fmt.Fprintf(w, "  %s %s: %s\n", i.Severity, i.Key, i.Message)
		}
	}
	if len(j.Diff) > 0 {
		fmt.Fprintln(w, "\nDrift")
		if err := writeDiff(w, j.Diff); err != nil {
			return err
		}
	}
	if len(j.Deployments) > 0 {
		fmt.Fprintln(w, "\nDeployments")
		writeDeployments(w, j.Deployments)
	}
	return nil
}
