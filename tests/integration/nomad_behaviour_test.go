//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
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
	job, err := c.ParseHCL(ctx, scalableHCL(id), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterCAS(ctx, job, 0, false); err != nil {
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
	if _, err := c.ParseHCL(ctx, mixed, ""); err == nil {
		t.Error("a job with meta as a block and as an object parsed: docs/meta-keys.md says it cannot")
	}

	object := `job "object" {
  meta = {
    "nops_managed" = "true"
    "diun.enable"  = "true"
  }` + body
	job, err := c.ParseHCL(ctx, object, "")
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

	parent, err := c.ParseHCL(ctx, fmt.Sprintf(`
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
	if _, err := c.RegisterCAS(ctx, parent, 0, false); err != nil {
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

	job, err := c.ParseHCL(ctx, batchHCL(id, "one"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterCAS(ctx, job, 0, false); err != nil {
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
		job, err := c.ParseHCL(ctx, canaryHCL(id, tag), "")
		if err != nil {
			t.Fatal(err)
		}
		live, _, err := raw.Jobs().Info(id, nil)
		var index uint64
		if err == nil {
			index = *live.JobModifyIndex
		}
		if _, err := c.RegisterCAS(ctx, job, index, false); err != nil {
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
		job, err := c.ParseHCL(ctx, canaryHCL(id, tag), "")
		if err != nil {
			t.Fatal(err)
		}
		var index uint64
		if live, _, err := raw.Jobs().Info(id, nil); err == nil {
			index = *live.JobModifyIndex
		}
		if _, err := c.RegisterCAS(ctx, job, index, false); err != nil {
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
