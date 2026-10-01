//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/nomadx"
)

// The tests of this file settle what docs/running-nops.md#the-nomad-token says the
// Nomad token needs. They need an agent with ACLs enabled, which the other
// tests do not run against: NOPS_TEST_NOMAD_ACL_ADDR is its address and
// NOPS_TEST_NOMAD_ACL_TOKEN a management token (the bootstrap one). Without
// them they are skipped.

// aclAdmin returns a management client of the ACL agent, raw and as nomadx.
func aclAdmin(t *testing.T) (*nomadx.Client, *api.Client) {
	t.Helper()
	addr, token := os.Getenv("NOPS_TEST_NOMAD_ACL_ADDR"), os.Getenv("NOPS_TEST_NOMAD_ACL_TOKEN")
	if addr == "" || token == "" {
		t.Skip("NOPS_TEST_NOMAD_ACL_ADDR or NOPS_TEST_NOMAD_ACL_TOKEN not set: skipping ACL test")
	}
	return aclClients(t, addr, token)
}

func aclClients(t *testing.T, addr, token string) (*nomadx.Client, *api.Client) {
	t.Helper()
	cfg := api.DefaultConfig()
	cfg.Address, cfg.SecretID = addr, token
	cfg.HttpClient = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	t.Cleanup(cfg.HttpClient.CloseIdleConnections)
	c, err := nomadx.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := api.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, raw
}

func randSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// tokenWith creates an ACL policy with rules and a client token holding it,
// both deleted when the test ends, and returns a nomadx client on that token.
func tokenWith(t *testing.T, admin *api.Client, rules string) (*nomadx.Client, *api.Client) {
	t.Helper()
	name := "nops-it-" + randSuffix(t)
	if _, err := admin.ACLPolicies().Upsert(&api.ACLPolicy{Name: name, Rules: rules}, nil); err != nil {
		t.Fatalf("create policy %s: %v", rules, err)
	}
	t.Cleanup(func() { admin.ACLPolicies().Delete(name, nil) })
	tok, _, err := admin.ACLTokens().Create(&api.ACLToken{Name: name, Type: "client", Policies: []string{name}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.ACLTokens().Delete(tok.AccessorID, nil) })
	return aclClients(t, admin.Address(), tok.SecretID)
}

// jobCaps is the namespace rule configuration.md gives the token.
const jobCaps = `namespace "default" {
  capabilities = ["list-jobs", "read-job", "submit-job", "dispatch-job"]
}
`

// volumeJobHCL is a job whose group asks for a volume and mounts it; the task
// never has to run, only the register is looked at.
func volumeJobHCL(id, volType, source string, readOnly bool) string {
	extra := ""
	if volType == "csi" {
		extra = `
      attachment_mode = "file-system"
      access_mode     = "single-node-writer"`
		if readOnly {
			extra = `
      attachment_mode = "file-system"
      access_mode     = "single-node-reader-only"`
		}
	}
	return fmt.Sprintf(`
job %q {
  type = "batch"
  group "g" {
    volume "v" {
      type      = %q
      source    = %q
      read_only = %t%s
    }
    task "t" {
      driver = "raw_exec"
      volume_mount {
        volume      = "v"
        destination = "/data"
        read_only   = %t
      }
      config {
        command = "/bin/true"
      }
    }
  }
}`, id, volType, source, readOnly, extra, readOnly)
}

// csiPluginJobHCL is a job with a task that declares itself a CSI plugin.
func csiPluginJobHCL(id string) string {
	return fmt.Sprintf(`
job %q {
  type = "service"
  group "g" {
    task "t" {
      driver = "raw_exec"
      csi_plugin {
        id        = "nops-it-plugin"
        type      = "node"
        mount_dir = "/csi"
      }
      config {
        command = "/bin/sh"
        args    = ["-c", "sleep 600"]
      }
    }
  }
}`, id)
}

// TestTokenACLForVolumes checks the table of running-nops.md#the-nomad-token: what a
// token needs, on top of the namespace rule, to register a job that mounts a
// volume, and that Plan checks none of it.
func TestTokenACLForVolumes(t *testing.T) {
	adminX, admin := aclAdmin(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	src := "nops-it-vol-" + randSuffix(t)
	for _, tc := range []struct {
		name     string
		rules    string
		hcl      func(id string) string
		register bool // false: refused with 403
	}{
		{"host volume, read-write, no volume rule", jobCaps,
			func(id string) string { return volumeJobHCL(id, "host", src, false) }, false},
		{"host volume, read-only, no volume rule", jobCaps,
			func(id string) string { return volumeJobHCL(id, "host", src, true) }, false},
		{"host volume, read-write, policy read", jobCaps + `host_volume "` + src + `" { policy = "read" }`,
			func(id string) string { return volumeJobHCL(id, "host", src, false) }, false},
		{"host volume, read-only, policy read", jobCaps + `host_volume "` + src + `" { policy = "read" }`,
			func(id string) string { return volumeJobHCL(id, "host", src, true) }, true},
		{"host volume, read-write, policy write", jobCaps + `host_volume "` + src + `" { policy = "write" }`,
			func(id string) string { return volumeJobHCL(id, "host", src, false) }, true},
		{"host volume, read-only, policy write", jobCaps + `host_volume "` + src + `" { policy = "write" }`,
			func(id string) string { return volumeJobHCL(id, "host", src, true) }, true},
		{"host volume with underscores, matched by a glob", jobCaps + `host_volume "nops*it*under" { policy = "write" }`,
			func(id string) string { return volumeJobHCL(id, "host", "nops_it_under", false) }, true},
		{"CSI volume, no csi-mount-volume", jobCaps,
			func(id string) string { return volumeJobHCL(id, "csi", src, false) }, false},
		{"CSI volume, csi-mount-volume only", `namespace "default" {
  capabilities = ["list-jobs", "read-job", "submit-job", "dispatch-job", "csi-mount-volume"]
}`, func(id string) string { return volumeJobHCL(id, "csi", src, false) }, false},
		{"CSI volume, plugin read only", jobCaps + `plugin { policy = "read" }`,
			func(id string) string { return volumeJobHCL(id, "csi", src, false) }, false},
		{"CSI volume, csi-mount-volume and plugin read", `namespace "default" {
  capabilities = ["list-jobs", "read-job", "submit-job", "dispatch-job", "csi-mount-volume"]
}
plugin { policy = "read" }`, func(id string) string { return volumeJobHCL(id, "csi", src, false) }, true},
		{"CSI plugin task, no csi-register-plugin", jobCaps, csiPluginJobHCL, false},
		{"CSI plugin task, csi-register-plugin", `namespace "default" {
  capabilities = ["list-jobs", "read-job", "submit-job", "dispatch-job", "csi-register-plugin"]
}`, csiPluginJobHCL, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := tokenWith(t, admin, tc.rules)
			id := uniqueID(t, admin, "acl")
			job, err := adminX.ParseHCL(ctx, "default", tc.hcl(id), "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Plan(ctx, job); err != nil {
				t.Errorf("Plan: %v, want it to pass: it checks no volume", err)
			}
			_, err = c.RegisterCAS(ctx, job, 0, false)
			switch {
			case tc.register && err != nil:
				t.Errorf("register: %v, want it accepted", err)
			case !tc.register && err == nil:
				t.Errorf("register accepted, want 403")
			case !tc.register && !strings.Contains(err.Error(), "403 (Permission denied)"):
				t.Errorf("register: %v, want 403 (Permission denied)", err)
			}
		})
	}
}

// TestACLPolicyVolumeNames checks what running-nops.md#the-nomad-token says of the
// name in a host_volume rule: letters, digits, "-" and "*" only.
func TestACLPolicyVolumeNames(t *testing.T) {
	_, admin := aclAdmin(t)
	for name, ok := range map[string]bool{
		"db-data": true,
		"db*data": true,
		"dbData2": true,
		"db_data": false,
		"db.data": false,
		"db/data": false,
	} {
		t.Run(name, func(t *testing.T) {
			pol := "nops-it-" + randSuffix(t)
			_, err := admin.ACLPolicies().Upsert(&api.ACLPolicy{Name: pol, Rules: `host_volume "` + name + `" { policy = "write" }`}, nil)
			if err == nil {
				t.Cleanup(func() { admin.ACLPolicies().Delete(pol, nil) })
			}
			if (err == nil) != ok {
				t.Errorf("policy with host_volume %q: err = %v, want accepted: %v", name, err, ok)
			}
		})
	}
}

// TestPromoteNeedsSubmitJob checks what running-nops.md#the-nomad-token says of the
// dashboard's Promote: a token with list-jobs and read-job only is refused
// with 403, one that also has submit-job promotes.
func TestPromoteNeedsSubmitJob(t *testing.T) {
	adminX, admin := aclAdmin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	id := uniqueID(t, admin, "aclpromote")
	register := func(tag string) {
		t.Helper()
		job, err := adminX.ParseHCL(ctx, "default", canaryHCL(id, tag), "")
		if err != nil {
			t.Fatal(err)
		}
		var index uint64
		if live, _, err := admin.Jobs().Info(id, nil); err == nil {
			index = *live.JobModifyIndex
		}
		if _, err := adminX.RegisterCAS(ctx, job, index, false); err != nil {
			t.Fatal(err)
		}
	}
	waitFor := func(what string, ok func(*api.Deployment) bool) *api.Deployment {
		t.Helper()
		end := time.Now().Add(60 * time.Second)
		for {
			d, err := adminX.LatestDeployment(ctx, "default", id)
			if err != nil {
				t.Fatal(err)
			}
			if d != nil && ok(d) {
				return d
			}
			if time.Now().After(end) {
				t.Fatalf("no Nomad deployment %s: %+v", what, d)
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	register("v1")
	waitFor("successful", func(d *api.Deployment) bool { return d.Status == api.DeploymentStatusSuccessful })
	register("v2")
	dep := waitFor("with a healthy canary", func(d *api.Deployment) bool {
		g := d.TaskGroups["g"]
		return d.Status == api.DeploymentStatusRunning && g != nil && g.HealthyAllocs >= 1
	})

	readOnly, _ := tokenWith(t, admin, `namespace "default" { capabilities = ["list-jobs", "read-job"] }`)
	err := readOnly.PromoteDeployment(ctx, "default", dep.ID)
	if err == nil || !strings.Contains(err.Error(), "403 (Permission denied)") {
		t.Errorf("promote with list-jobs and read-job: %v, want 403 (Permission denied)", err)
	}
	submit, _ := tokenWith(t, admin, `namespace "default" { capabilities = ["list-jobs", "read-job", "submit-job"] }`)
	if err := submit.PromoteDeployment(ctx, "default", dep.ID); err != nil {
		t.Errorf("promote with submit-job: %v, want it promoted", err)
	}
}

// TestNomadPanelReadsWithReadJob checks what dashboard.md#the-nomad-panel says
// the panel needs: the three reads it makes (the job, its allocations, its
// latest Nomad deployment) pass with list-jobs and read-job.
func TestNomadPanelReadsWithReadJob(t *testing.T) {
	adminX, admin := aclAdmin(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	id := uniqueID(t, admin, "aclpanel")
	job, err := adminX.ParseHCL(ctx, "default", canaryHCL(id, "v1"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adminX.RegisterCAS(ctx, job, 0, false); err != nil {
		t.Fatal(err)
	}

	c, _ := tokenWith(t, admin, `namespace "default" { capabilities = ["list-jobs", "read-job"] }`)
	if _, err := c.Job(ctx, "default", id); err != nil {
		t.Errorf("Job: %v", err)
	}
	if _, err := c.Allocations(ctx, "default", id); err != nil {
		t.Errorf("Allocations: %v", err)
	}
	if _, err := c.LatestDeployment(ctx, "default", id); err != nil {
		t.Errorf("LatestDeployment: %v", err)
	}
}

// TestParseNeedsNoRuleInDefault checks that parsing a job, which Nops does for
// every file of the repository, is checked in a namespace Nops manages and not
// in "default": a token whose only rule is in a listed namespace parses.
func TestParseNeedsNoRuleInDefault(t *testing.T) {
	_, admin := aclAdmin(t)
	ns := newNamespace(t, admin, "aclparse")
	c, _ := tokenWith(t, admin, `namespace "`+ns+`" {
  capabilities = ["list-jobs", "read-job", "submit-job", "dispatch-job"]
}
`)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	id := uniqueID(t, admin, "aclparse")
	if _, err := c.ParseHCL(ctx, ns, inNamespace(batchHCL(id, "one"), ns), ""); err != nil {
		t.Errorf("ParseHCL with a rule in %s only: %v, want it parsed", ns, err)
	}
}
