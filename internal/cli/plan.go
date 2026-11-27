package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/music-gang/nops/internal/gitwatch"
)

type planFlags struct{ repository, commit string }

// planTimeout bounds a plan: it waits for the plans before it, and Nomad
// parses and plans every job of the repository.
const planTimeout = 10 * time.Minute

func planFlagSet(fs *flag.FlagSet, k *call) {
	fs.StringVar(&k.plan.repository, "repository", "", "name of the repository block the directory is a checkout of (required)")
	fs.StringVar(&k.plan.commit, "commit", "", "full SHA of the commit, for nops_commit (required)")
}

type planFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type plannedJob struct {
	job
	Repository string   `json:"repository"`
	File       string   `json:"file"`
	PreHooks   []string `json:"pre_hooks"`
	PostHooks  []string `json:"post_hooks"`
	Issues     []struct {
		Severity string `json:"severity"`
		Key      string `json:"key"`
		Message  string `json:"message"`
	} `json:"issues"`
	Diff  json.RawMessage `json:"diff"`
	Error string          `json:"error"`
}

type planAnswer struct {
	Jobs   []plannedJob `json:"jobs"`
	Errors []struct {
		File      string `json:"file"`
		Namespace string `json:"namespace"`
		Job       string `json:"job"`
		Error     string `json:"error"`
	} `json:"errors"`
	Removed []struct {
		Namespace  string `json:"namespace"`
		Job        string `json:"job"`
		Superseded string `json:"superseded"`
		Orphan     bool   `json:"orphan"`
	} `json:"removed"`
	RemovedUnchecked bool `json:"removed_unchecked"`
}

// runPlan sends the job files and var-files of the directory, a checkout of
// the repository, and prints what Nops would do with them. It fails when a
// job would not deploy for an error in the files.
func runPlan(k *call) error {
	if k.plan.repository == "" || k.plan.commit == "" {
		return usageErrorf("-repository and -commit are required")
	}
	files, err := readCheckout(k.arg)
	if err != nil {
		return err
	}
	patient := *k.client
	hc := *k.client.http
	hc.Timeout = planTimeout
	patient.http = &hc
	var p planAnswer
	raw, err := patient.do(k.ctx, http.MethodPost, "/api/plan", map[string]any{"repository": k.plan.repository, "commit": k.plan.commit, "files": files})
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("read the answer of the API: %w", err)
	}
	if k.asJSON {
		writeJSON(k.out, raw)
	} else if err := writePlan(k.out, k.plan.repository, p); err != nil {
		return err
	}
	if n := planErrors(p); n > 0 {
		return fmt.Errorf("%d %s would keep a job from deploying", n, plural(n, "error", "errors"))
	}
	return nil
}

// readCheckout reads the files of dir a snapshot reads, by their path from
// dir. The .git directory, and anything but a regular file, are left out:
// nothing is read outside dir.
func readCheckout(dir string) ([]planFile, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	defer root.Close()
	files := []planFile{}
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		if !d.Type().IsRegular() || !gitwatch.Wanted(d.Name()) {
			return nil
		}
		content, err := root.ReadFile(p)
		if err != nil {
			return err
		}
		files = append(files, planFile{Path: p, Content: string(content)})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	return files, nil
}

// planErrors counts what would keep a job from deploying.
func planErrors(p planAnswer) int {
	n := len(p.Errors)
	for _, j := range p.Jobs {
		if j.Error != "" {
			n++
		}
	}
	return n
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// writePlan prints the plan of repo. A job of another repository is one that
// runs a hook of it: its repository is printed.
func writePlan(w io.Writer, repo string, p planAnswer) error {
	if len(p.Jobs) == 0 && len(p.Errors) == 0 && len(p.Removed) == 0 {
		fmt.Fprintln(w, "No jobs.")
	}
	for i, j := range p.Jobs {
		if i > 0 {
			fmt.Fprintln(w)
		}
		field(w, "Job", j.Namespace+"/"+j.Job)
		field(w, "Policy", j.Policy)
		field(w, "Sync", j.Sync)
		if j.Repository != repo {
			field(w, "Repository", j.Repository)
		}
		field(w, "File", j.File)
		field(w, "Pre-hooks", strings.Join(j.PreHooks, ", "))
		field(w, "Post-hooks", strings.Join(j.PostHooks, ", "))
		field(w, "Blocked by", j.BlockedBy)
		if h := j.Hold; h != nil {
			field(w, "Held", h.Kind+": "+h.Reason)
		}
		field(w, "Error", j.Error)
		for _, iss := range j.Issues {
			fmt.Fprintf(w, "  %s %s: %s\n", iss.Severity, iss.Key, iss.Message)
		}
		if len(j.Diff) > 0 {
			fmt.Fprintln(w)
			if err := writeDiff(w, j.Diff); err != nil {
				return err
			}
		}
	}
	if len(p.Removed) > 0 {
		fmt.Fprintln(w, "\nRemoved from the repository")
		t := table(w)
		for _, r := range p.Removed {
			var what []string
			if r.Superseded != "" {
				what = append(what, "supersedes "+r.Superseded)
			}
			if r.Orphan {
				what = append(what, "becomes an orphan")
			}
			fmt.Fprintf(t, "  %s/%s\t%s\n", r.Namespace, r.Job, dash(strings.Join(what, ", ")))
		}
		t.Flush()
	}
	if p.RemovedUnchecked {
		fmt.Fprintln(w, "\nA file does not parse: the jobs the change removes are not checked.")
	}
	if len(p.Errors) > 0 {
		fmt.Fprintln(w, "\nErrors")
		t := table(w)
		for _, e := range p.Errors {
			what := ""
			if e.Job != "" {
				what = e.Namespace + "/" + e.Job
			}
			fmt.Fprintf(t, "  %s\t%s\t%s\n", dash(e.File), dash(what), e.Error)
		}
		t.Flush()
	}
	return nil
}
