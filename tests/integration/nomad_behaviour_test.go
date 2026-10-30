//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
)

// scalableHCL is a service job whose only group has a scaling policy, the
// shape an autoscaler owns. It has no rolling update (max_parallel = 0), so
// Nomad creates no deployment for it: while one is active Nomad refuses to
// scale the job ("job scaling blocked due to active deployment"), which would
// make the test depend on timing. The task is never waited for: the tests only
// read what Nomad records about the job.
func scalableHCL(id string) string {
	return `
job "` + id + `" {
  type = "service"
  update {
    max_parallel = 0
  }
  group "g" {
    count = 1
    scaling {
      min     = 1
      max     = 10
      enabled = true
    }
    task "t" {
      driver = "raw_exec"
      config {
        command = "/bin/sh"
        args    = ["-c", "sleep 600"]
      }
    }
  }
}`
}

// TestScaleChangesTheLiveJobsIndex settles what docs/archive/engine-detection.md
// says an autoscaler never does to nops: does moving a count through Nomad's
// scale API change the live job's JobModifyIndex (what nops keeps as cas_index)
// and its Version? Detection supersedes a pending deployment when the index
// moves (reconcileDeployment), so the answer decides whether "an autoscaler
// never supersedes a pending approval" can hold.
func TestScaleChangesTheLiveJobsIndex(t *testing.T) {
	c, raw := newClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id := uniqueID(t, raw, "scale")
	job, err := c.ParseHCL(ctx, "default", scalableHCL(id), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterCAS(ctx, job, 0, false, nil); err != nil {
		t.Fatal(err)
	}
	before, err := c.Job(ctx, "default", id)
	if err != nil {
		t.Fatal(err)
	}

	count := 3
	if _, _, err := raw.Jobs().Scale(id, "g", &count, "moved by the test", false, nil, nil); err != nil {
		t.Fatalf("scale: %v", err)
	}
	after, err := c.Job(ctx, "default", id)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("before: count=%d JobModifyIndex=%d ModifyIndex=%d Version=%d",
		*before.TaskGroups[0].Count, *before.JobModifyIndex, *before.ModifyIndex, *before.Version)
	t.Logf("after:  count=%d JobModifyIndex=%d ModifyIndex=%d Version=%d",
		*after.TaskGroups[0].Count, *after.JobModifyIndex, *after.ModifyIndex, *after.Version)

	if got := *after.TaskGroups[0].Count; got != count {
		t.Fatalf("count after the scale = %d, want %d", got, count)
	}
	// Nomad treats a scale as a new job version, so it moves the
	// index nops compares (cas_index) whether or not the spec changed.
	if *after.JobModifyIndex <= *before.JobModifyIndex {
		t.Errorf("JobModifyIndex %d -> %d: a scale is expected to move it", *before.JobModifyIndex, *after.JobModifyIndex)
	}
	if *after.Version <= *before.Version {
		t.Errorf("Version %d -> %d: a scale is expected to bump it", *before.Version, *after.Version)
	}
}

// TestMetaBlockAndObjectFormsCannotBeMixed checks what docs/meta-keys.md says
// about a job that also needs meta keys with dots: HCL does not allow the
// block form and the object form for the same attribute, so the whole meta
// has to be written as an object.
func TestMetaBlockAndObjectFormsCannotBeMixed(t *testing.T) {
	c, _ := newClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const body = `
  group "g" {
    task "t" {
      driver = "raw_exec"
      config {
        command = "/bin/true"
      }
    }
  }
}`
	mixed := `job "mixed" {
  meta {
    nops_managed = "true"
  }
  meta = {
    "diun.enable" = "true"
  }` + body
	if _, err := c.ParseHCL(ctx, "default", mixed, ""); err == nil {
		t.Error("a job with meta as a block and as an object parsed: docs/meta-keys.md says it cannot")
	}

	object := `job "object" {
  meta = {
    "nops_managed" = "true"
    "diun.enable"  = "true"
  }` + body
	job, err := c.ParseHCL(ctx, "default", object, "")
	if err != nil {
		t.Fatalf("meta as one object: %v", err)
	}
	if job.Meta["nops_managed"] != "true" || job.Meta["diun.enable"] != "true" {
		t.Errorf("meta = %v, want both keys", job.Meta)
	}
}

// TestStoppingAHookRevisionLeavesItsRunningRun checks what
// docs/archive/engine-detection.md relies on when it garbage collects a hook
// revision: deregistering the parameterized parent without purge does not take
// a run it dispatched away, even one still running, so a revision can be
// stopped while its run is being read.
func TestStoppingAHookRevisionLeavesItsRunningRun(t *testing.T) {
	c, raw := newClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	id := uniqueID(t, raw, "gcrun")

	parent, err := c.ParseHCL(ctx, "default", fmt.Sprintf(`
job %q {
  type = "batch"
  parameterized {}
  group "g" {
    task "t" {
      driver = "raw_exec"
      config {
        command = "/bin/sh"
        args    = ["-c", "sleep 600"]
      }
    }
  }
}`, id), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterCAS(ctx, parent, 0, false, nil); err != nil {
		t.Fatal(err)
	}
	run, err := c.Dispatch(ctx, "default", id, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for {
		allocs, _, err := raw.Jobs().Allocations(run.JobID, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(allocs) > 0 && allocs[0].ClientStatus == "running" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the run %s never started", run.JobID)
		case <-time.After(200 * time.Millisecond):
		}
	}

	if err := c.StopJob(ctx, "default", id); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Job(ctx, "default", run.JobID); err != nil {
		t.Errorf("the run %s cannot be read once its parent was stopped: %v", run.JobID, err)
	}
	allocs, _, err := raw.Jobs().Allocations(run.JobID, true, nil)
	if err != nil || len(allocs) == 0 {
		t.Errorf("allocations of the run = %d, %v; want them readable", len(allocs), err)
	}
}

// TestLongJobIDIsAccepted checks the claim of docs/archive/engine-detection.md
// that the "-<8 hex>" suffix of a hook revision hits no limit on the length of
// a job ID: an ID of several hundred characters registers and reads back.
func TestLongJobIDIsAccepted(t *testing.T) {
	c, raw := newClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id := uniqueID(t, raw, "long")
	id += strings.Repeat("x", 400-len(id)) + "-0a1b2c3d"

	job, err := c.ParseHCL(ctx, "default", batchHCL(id, "one"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterCAS(ctx, job, 0, false, nil); err != nil {
		t.Fatalf("register with an ID of %d characters: %v", len(id), err)
	}
	live, err := c.Job(ctx, "default", id)
	if err != nil || live.ID == nil || *live.ID != id {
		t.Fatalf("live = %+v, %v; want the job back under its long ID", live, err)
	}
}

// canaryHCL is a service with one canary and no auto-promotion: what a job
// needs for its Nomad deployment to wait for a human. tag changes the task's
// environment, so a second registration is a new version. The deadlines are
// short so the test does not take long, but the progress deadline is shorter
// than the wait the test makes.
func canaryHCL(id, tag string) string {
	return fmt.Sprintf(`
job %q {
  type = "service"
  update {
    max_parallel      = 1
    canary            = 1
    auto_promote      = false
    min_healthy_time  = "1s"
    healthy_deadline  = "10s"
    progress_deadline = "15s"
  }
  group "g" {
    count = 1
    task "t" {
      driver = "raw_exec"
      env {
        TAG = %q
      }
      config {
        command = "/bin/sh"
        args    = ["-c", "sleep 600"]
      }
    }
  }
}`, id, tag)
}

// TestCanaryWaitsForManualPromotion settles what docs/archive/engine-apply.md
// relies on for a job with canary > 0 and auto_promote = false: its Nomad
// deployment stays running, with the canaries placed and healthy and not
// promoted, and the progress deadline does not fail it while it waits for a
// human. Promoting it makes it successful.
func TestCanaryWaitsForManualPromotion(t *testing.T) {
	c, raw := newClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	id := uniqueID(t, raw, "canary")
	register := func(tag string) {
		t.Helper()
		job, err := c.ParseHCL(ctx, "default", canaryHCL(id, tag), "")
		if err != nil {
			t.Fatal(err)
		}
		live, _, err := raw.Jobs().Info(id, nil)
		var index uint64
		if err == nil {
			index = *live.JobModifyIndex
		}
		if _, err := c.RegisterCAS(ctx, job, index, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	latest := func() *api.Deployment {
		t.Helper()
		d, err := c.LatestDeployment(ctx, "default", id)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	waitFor := func(what string, deadline time.Duration, ok func(*api.Deployment) bool) *api.Deployment {
		t.Helper()
		end := time.Now().Add(deadline)
		for {
			if d := latest(); d != nil && ok(d) {
				return d
			}
			if time.Now().After(end) {
				t.Fatalf("no Nomad deployment %s within %s: %+v", what, deadline, latest())
			}
			time.Sleep(300 * time.Millisecond)
		}
	}

	// The first version has nothing to be a canary of: it rolls out whole.
	register("v1")
	waitFor("successful", 60*time.Second, func(d *api.Deployment) bool { return d.Status == api.DeploymentStatusSuccessful })

	register("v2")
	waiting := waitFor("with a healthy canary", 60*time.Second, func(d *api.Deployment) bool {
		g := d.TaskGroups["g"]
		return d.Status == api.DeploymentStatusRunning && g != nil && g.HealthyAllocs >= 1
	})
	g := waiting.TaskGroups["g"]
	t.Logf("waiting: status=%q description=%q group=%+v", waiting.Status, waiting.StatusDescription, *g)
	if g.DesiredCanaries != 1 || len(g.PlacedCanaries) != 1 || g.Promoted {
		t.Errorf("group = %+v, want 1 desired canary, 1 placed, not promoted", *g)
	}

	// Past the progress deadline: nothing is failing, a human is expected.
	time.Sleep(20 * time.Second)
	still := latest()
	t.Logf("after the progress deadline: status=%q description=%q", still.Status, still.StatusDescription)
	if still.ID != waiting.ID || still.Status != api.DeploymentStatusRunning {
		t.Fatalf("after the progress deadline the deployment is %s %q (%s), want %s still running",
			still.ID, still.Status, still.StatusDescription, waiting.ID)
	}

	if _, _, err := raw.Deployments().PromoteAll(waiting.ID, nil); err != nil {
		t.Fatalf("promote: %v", err)
	}
	done := waitFor("successful after the promotion", 60*time.Second, func(d *api.Deployment) bool {
		return d.Status == api.DeploymentStatusSuccessful
	})
	if !done.TaskGroups["g"].Promoted {
		t.Errorf("group of the successful deployment = %+v, want promoted", *done.TaskGroups["g"])
	}
}

// TestPromoteDeploymentAndTheAllocationFieldsThePanelReads runs nomadx against
// a real Nomad with a canary that waits for a promotion: Allocations says which
// group an allocation is in, that it is a canary and that its Nomad deployment
// found it healthy, PromoteDeployment promotes it, and Nomad refuses to promote
// a deployment that has nothing left to promote.
func TestPromoteDeploymentAndTheAllocationFieldsThePanelReads(t *testing.T) {
	c, raw := newClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	id := uniqueID(t, raw, "promote")
	register := func(tag string) {
		t.Helper()
		job, err := c.ParseHCL(ctx, "default", canaryHCL(id, tag), "")
		if err != nil {
			t.Fatal(err)
		}
		var index uint64
		if live, _, err := raw.Jobs().Info(id, nil); err == nil {
			index = *live.JobModifyIndex
		}
		if _, err := c.RegisterCAS(ctx, job, index, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	waitFor := func(what string, ok func(*api.Deployment) bool) *api.Deployment {
		t.Helper()
		end := time.Now().Add(60 * time.Second)
		for {
			d, err := c.LatestDeployment(ctx, "default", id)
			if err != nil {
				t.Fatal(err)
			}
			if d != nil && ok(d) {
				return d
			}
			if time.Now().After(end) {
				t.Fatalf("no Nomad deployment %s in time: %+v", what, d)
			}
			time.Sleep(300 * time.Millisecond)
		}
	}

	register("v1")
	waitFor("successful", func(d *api.Deployment) bool { return d.Status == api.DeploymentStatusSuccessful })
	register("v2")
	waiting := waitFor("with a healthy canary", func(d *api.Deployment) bool {
		g := d.TaskGroups["g"]
		return d.Status == api.DeploymentStatusRunning && g != nil && g.HealthyAllocs >= 1
	})

	allocs, err := c.Allocations(ctx, "default", id)
	if err != nil {
		t.Fatal(err)
	}
	var canaries int
	for _, a := range allocs {
		if a.Canary {
			canaries++
			if a.TaskGroup != "g" || a.Healthy == nil || !*a.Healthy {
				t.Errorf("canary allocation = %+v, want group g and healthy", a)
			}
		}
	}
	if canaries != 1 {
		t.Fatalf("%d canary allocations in %+v, want 1", canaries, allocs)
	}

	if err := c.PromoteDeployment(ctx, "default", waiting.ID); err != nil {
		t.Fatalf("PromoteDeployment: %v", err)
	}
	done := waitFor("successful after the promotion", func(d *api.Deployment) bool { return d.Status == api.DeploymentStatusSuccessful })
	if !done.TaskGroups["g"].Promoted {
		t.Errorf("group = %+v, want promoted", *done.TaskGroups["g"])
	}

	if err := c.PromoteDeployment(ctx, "default", done.ID); err == nil {
		t.Error("promoting a finished deployment succeeded: Nomad is expected to refuse it")
	} else {
		t.Logf("Nomad on promoting a finished deployment: %v", err)
	}
}

// TestNomadUIServesTheLinkedRoutes checks what the dashboard's links into the
// Nomad UI rely on (docs/dashboard.md#the-nomad-panel): a job is at
// /ui/jobs/<id>@<namespace> and its deployments at .../deployments. The UI is a
// JavaScript app that answers every /ui/ path with the same page, so a request
// alone proves nothing about a route: the test reads the routes out of the
// application's own bundle, which is where Nomad defines them. A Nomad version
// that renames them fails here, in the CI that tests against it.
func TestNomadUIServesTheLinkedRoutes(t *testing.T) {
	base := testAddr(t)
	get := func(path string) (int, string, string) {
		t.Helper()
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
	}

	for _, path := range []string{"/ui/", "/ui/jobs/web@default", "/ui/jobs/web@default/deployments"} {
		status, ctype, body := get(path)
		if status != http.StatusOK || !strings.HasPrefix(ctype, "text/html") || !strings.Contains(body, "nomad-ui") {
			t.Errorf("GET %s = %d %s, want the UI's page", path, status, ctype)
		}
	}

	_, _, shell := get("/ui/")
	// The UI lives under /ui/ and routes by the path (not the fragment).
	for _, want := range []string{"%22rootURL%22%3A%22%2Fui%2F%22", "%22locationType%22%3A%22history%22"} {
		if !strings.Contains(shell, want) {
			t.Errorf("the UI's page does not carry %q: its URLs are not /ui/<route> any more", want)
		}
	}
	m := regexp.MustCompile(`src="(/ui/assets/nomad-ui-[0-9a-f]+\.js)"`).FindStringSubmatch(shell)
	if m == nil {
		t.Fatal("the UI's page references no nomad-ui-<hash>.js: where its routes are defined moved")
	}
	status, _, bundle := get(m[1])
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d", m[1], status)
	}
	for what, want := range map[string]string{
		"a job is a route of jobs, by name":        `this.route("job",{path:"/:job_name"}`,
		"a job has a deployments tab":              `this.route("deployments")`,
		"the name is <id>@<namespace> in one part": `lastIndexOf("@")`,
	} {
		if !strings.Contains(bundle, want) {
			t.Errorf("the UI's bundle does not contain %s (%s): the links of the dashboard may be wrong for this Nomad", want, what)
		}
	}
}

// sourceHCL is a batch job that differs between tags in its meta only.
func sourceHCL(id, tag string) string {
	return `
job "` + id + `" {
  type = "batch"
  meta {
    tag = "` + tag + `"
  }
  group "g" {
    task "t" {
      driver = "raw_exec"
      config {
        command = "/bin/sh"
        args    = ["-c", "exit 0"]
      }
    }
  }
}`
}

// submissionOf is the source Nomad keeps with a version of a job, nil if it
// keeps none (it answers 404).
func submissionOf(t *testing.T, raw *api.Client, id string, version uint64) *api.JobSubmission {
	t.Helper()
	sub, _, err := raw.Jobs().Submission(id, int(version), &api.QueryOptions{Namespace: "default"})
	var ue api.UnexpectedResponseError
	if errors.As(err, &ue) && ue.StatusCode() == http.StatusNotFound {
		return nil
	}
	if err != nil {
		t.Fatalf("submission of %s v%d: %v", id, version, err)
	}
	return sub
}

// TestRegisterWithoutSourceAtTheNextVersion pins what makes nops register a job
// at the live version plus one: Nomad takes a register without a source at a
// version that isn't above the live one for a revert, and attaches the source
// of that version, so a job first submitted with HCL shows its first file for
// every version nops registers (`nomad job inspect -hcl`).
func TestRegisterWithoutSourceAtTheNextVersion(t *testing.T) {
	c, raw := newClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id := uniqueID(t, raw, "source")
	register := func(tag string, version *uint64, sub *api.JobSubmission) *api.Job {
		t.Helper()
		job, err := c.ParseHCL(ctx, "default", sourceHCL(id, tag), "")
		if err != nil {
			t.Fatal(err)
		}
		live, err := c.Job(ctx, "default", id)
		var index uint64
		if err == nil {
			index = *live.JobModifyIndex
		}
		if version != nil {
			job.Version = version
		}
		if _, err := c.RegisterCAS(ctx, job, index, false, sub); err != nil {
			t.Fatalf("register %s: %v", tag, err)
		}
		got, err := c.Job(ctx, "default", id)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	first := sourceHCL(id, "one")
	v0 := register("one", nil, &api.JobSubmission{Source: first, Format: "hcl2"})
	if sub := submissionOf(t, raw, id, *v0.Version); sub == nil || sub.Source != first {
		t.Fatalf("setup: the source of v%d is %+v, want the file it was submitted with", *v0.Version, sub)
	}

	// At version 0, as a parsed job is: the revert branch.
	reverted := register("two", nil, nil)
	if sub := submissionOf(t, raw, id, *reverted.Version); sub == nil || sub.Source != first {
		t.Errorf("v%d registered at version 0 has source %+v, want the first file (Nomad's revert branch)", *reverted.Version, sub)
	}

	// At the live version plus one: no source of another version.
	next := *reverted.Version + 1
	got := register("three", &next, nil)
	if *got.Version != next {
		t.Fatalf("version = %d, want %d", *got.Version, next)
	}
	if sub := submissionOf(t, raw, id, *got.Version); sub != nil {
		t.Errorf("v%d registered at the live version plus one has source %+v, want none", *got.Version, sub)
	}
}

// serviceUpdateHCL is a raw_exec service with the given update block ("" for
// none). tag is what a second register changes.
func serviceUpdateHCL(id, update, tag string) string {
	return fmt.Sprintf(`
job %q {
  type = "service"
%s
  group "g" {
    count = 1
    task "t" {
      driver = "raw_exec"
      env {
        TAG = %q
      }
      config {
        command = "/bin/sh"
        args    = ["-c", "sleep 600"]
      }
    }
  }
}`, id, update, tag)
}

// TestWhichServiceRegistersGetANomadDeployment settles which service jobs take
// the allocations path of apply health: those whose register gets no Nomad
// deployment tracking its index. A service without an update block takes
// Nomad's default rolling update, so its registers get one; max_parallel = 0
// turns rolling updates off, and with them the deployments.
func TestWhichServiceRegistersGetANomadDeployment(t *testing.T) {
	for _, tc := range []struct {
		name, update   string
		wantDeployment bool
	}{
		{"no update block", "", true},
		{"max_parallel = 0", "  update {\n    max_parallel = 0\n  }", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, raw := newClient(t)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			id := uniqueID(t, raw, "svcdep")

			var index uint64
			for _, tag := range []string{"one", "two"} {
				job, err := c.ParseHCL(ctx, "default", serviceUpdateHCL(id, tc.update, tag), "")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := c.RegisterCAS(ctx, job, index, false, nil); err != nil {
					t.Fatalf("register %s: %v", tag, err)
				}
				live, err := c.Job(ctx, "default", id)
				if err != nil {
					t.Fatal(err)
				}
				index = *live.JobModifyIndex

				// The scheduler has run once the version has an allocation; the
				// deployment, if any, is created by the same evaluation.
				deadline := time.Now().Add(30 * time.Second)
				for {
					allocs, err := c.Allocations(ctx, "default", id)
					if err != nil {
						t.Fatal(err)
					}
					placed := false
					for _, a := range allocs {
						placed = placed || a.JobVersion == *live.Version
					}
					if placed {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("%s: no allocation of version %d", tag, *live.Version)
					}
					time.Sleep(200 * time.Millisecond)
				}
				dep, err := c.LatestDeployment(ctx, "default", id)
				if err != nil {
					t.Fatal(err)
				}
				tracks := dep != nil && dep.JobModifyIndex == index
				if tracks != tc.wantDeployment {
					t.Errorf("%s: Nomad deployment tracking index %d = %v (latest %+v), want %v", tag, index, tracks, dep, tc.wantDeployment)
				}
			}
		})
	}
}

// startClient runs a second Nomad client, named name, against the leader of
// the agent under test, and returns its node ID once it is ready. The client is
// killed and its node purged when the test ends.
func startClient(t *testing.T, raw *api.Client, name string) (nodeID string, kill func()) {
	t.Helper()
	leader, err := raw.Status().Leader()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := fmt.Sprintf(`
name      = %[1]q
data_dir  = %[2]q
bind_addr = "127.0.0.1"
ports {
  http = %[3]d
  rpc  = %[4]d
  serf = %[5]d
}
advertise {
  http = "127.0.0.1:%[3]d"
  rpc  = "127.0.0.1:%[4]d"
  serf = "127.0.0.1:%[5]d"
}
client {
  enabled      = true
  servers      = [%[6]q]
  no_host_uuid = true
}
plugin "raw_exec" {
  config {
    enabled = true
  }
}
`, name, filepath.Join(dir, "data"), freePort(t), freePort(t), freePort(t), leader)
	cfgPath := filepath.Join(dir, "client.hcl")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(dir, "client.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("nomad", "agent", "-config", cfgPath)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	// Its own process group, so the kill takes its executors with it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start a second Nomad client: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	kill = func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
		exited <- nil // a second kill returns at once
	}
	t.Cleanup(func() {
		kill()
		// The executors of its tasks leave the group and outlive the client:
		// their command lines name its data directory.
		_ = exec.Command("pkill", "-KILL", "-f", dir).Run()
		logFile.Close()
		if nodeID != "" {
			_, _, _ = raw.Nodes().Purge(nodeID, nil)
		}
	})

	deadline := time.Now().Add(60 * time.Second)
	for {
		nodes, _, err := raw.Nodes().List(nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range nodes {
			if n.Name == name && n.Status == api.NodeStatusReady {
				return n.ID, kill
			}
		}
		select {
		case err := <-exited:
			exited <- err
			log, _ := os.ReadFile(logFile.Name())
			t.Fatalf("the second client exited (%v):\n%s", err, log)
		default:
		}
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(logFile.Name())
			t.Fatalf("the second client did not join in time:\n%s", log)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestALostAllocationNamesItsReplacement settles what apply health reads of a
// lost allocation: when its node goes down, Nomad places a replacement on
// another node and records it as the lost allocation's NextAllocation.
func TestALostAllocationNamesItsReplacement(t *testing.T) {
	c, raw := newClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	id := uniqueID(t, raw, "lost")
	nodeName := id + "-node"
	nodeID, kill := startClient(t, raw, nodeName)

	// No rolling update, so no Nomad deployment: the allocations path of apply
	// health. The affinity puts the allocation on the node that goes away. The
	// task of the killed client outlives it (raw_exec gives it a process group
	// of its own), so it sleeps no longer than the test needs.
	hcl := fmt.Sprintf(`
job %q {
  type = "service"
  update {
    max_parallel = 0
  }
  group "g" {
    count = 1
    affinity {
      attribute = "${node.unique.name}"
      value     = %q
      weight    = 100
    }
    task "t" {
      driver = "raw_exec"
      config {
        command = "/bin/sh"
        args    = ["-c", "sleep 60"]
      }
    }
  }
}`, id, nodeName)
	job, err := c.ParseHCL(ctx, "default", hcl, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterCAS(ctx, job, 0, false, nil); err != nil {
		t.Fatal(err)
	}

	// waitAlloc waits for an allocation that ok accepts.
	waitAlloc := func(what string, timeout time.Duration, ok func(*api.AllocationListStub) bool) *api.AllocationListStub {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for {
			stubs, _, err := raw.Jobs().Allocations(id, true, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range stubs {
				if ok(s) {
					return s
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("no allocation %s in %s: %+v", what, timeout, stubs)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	first := waitAlloc("running on the second node", 60*time.Second, func(s *api.AllocationListStub) bool {
		return s.ClientStatus == "running"
	})
	if first.NodeID != nodeID {
		t.Fatalf("setup: the allocation runs on node %s, want the second client %s", first.NodeName, nodeName)
	}

	kill()

	lost := waitAlloc("lost", 2*time.Minute, func(s *api.AllocationListStub) bool {
		return s.ID == first.ID && s.ClientStatus == "lost"
	})
	if lost.NextAllocation == "" {
		// Nomad may record the replacement a moment after the loss.
		lost = waitAlloc("lost with a replacement", 30*time.Second, func(s *api.AllocationListStub) bool {
			return s.ID == first.ID && s.NextAllocation != ""
		})
	}
	replacement := waitAlloc("replacing the lost one", 60*time.Second, func(s *api.AllocationListStub) bool {
		return s.ID == lost.NextAllocation && s.ClientStatus == "running"
	})
	if replacement.NodeID == nodeID || replacement.JobVersion != lost.JobVersion {
		t.Errorf("replacement = node %s version %d, want another node and version %d", replacement.NodeName, replacement.JobVersion, lost.JobVersion)
	}
}
