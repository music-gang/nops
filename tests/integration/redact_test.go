//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/music-gang/nops/internal/redact"
)

// probeHCL puts a secret in every place the redaction rules cover, plus
// values that are not secrets. v prefixes every value, so planning v="new"
// over v="old" edits all of them. count = 0: nothing is ever placed.
// internal/redact/testdata/plan_diff.json is this plan, with the job ID
// "probe".
func probeHCL(id, v string) string {
	return fmt.Sprintf(`
job %[1]q {
  meta {
    api_token = "%[2]s-meta-token"
    owner     = "%[2]s-owner"
  }
  group "g" {
    count = 0
    network {
      port "http" {}
    }
    service {
      name     = "probe"
      port     = "http"
      provider = "nomad"
      check {
        type     = "http"
        path     = "/health"
        interval = "10s"
        timeout  = "2s"
        header {
          Authorization = ["Bearer %[2]s-check-header"]
        }
      }
    }
    task "t" {
      driver = "docker"
      config {
        image = "busybox:%[2]s"
        args  = ["--db-password=%[2]s-arg"]
        auth {
          username = "%[2]s-user"
          password = "%[2]s-docker-password"
        }
      }
      env {
        DB_PASSWORD = "%[2]s-env-secret"
        PLAIN       = "%[2]s-env-plain"
      }
      template {
        data        = "PASS=%[2]s-template-body"
        destination = "local/x.env"
        env         = true
      }
      artifact {
        source = "https://example.com/%[2]s.tar.gz"
        headers {
          X-Api-Key = "%[2]s-artifact-header"
        }
        options {
          ref = "%[2]s-getter-opt"
        }
      }
    }
  }
}`, id, v)
}

// TestRedactRealPlan checks the redaction rules against the field names of a
// real plan diff, not only against hand-built ones.
func TestRedactRealPlan(t *testing.T) {
	c, raw := newClient(t)
	ctx := context.Background()
	id := uniqueID(t, raw, "redact")

	v1, err := c.ParseHCL(ctx, probeHCL(id, "old"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegisterCAS(ctx, v1, 0, false); err != nil {
		t.Fatal(err)
	}
	v2, err := c.ParseHCL(ctx, probeHCL(id, "new"), "")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := c.Plan(ctx, v2)
	if err != nil {
		t.Fatal(err)
	}
	out, err := redact.Diff(plan.Diff)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, v := range []string{"old", "new"} {
		for _, secret := range []string{"meta-token", "check-header", "docker-password", "user", "env-secret", "env-plain", "template-body", "artifact-header"} {
			if strings.Contains(s, v+"-"+secret) {
				t.Errorf("secret %s-%s reached the redacted diff", v, secret)
			}
		}
		for _, kept := range []string{v + "-owner", "busybox:" + v, v + "-getter-opt"} {
			if !strings.Contains(s, kept) {
				t.Errorf("%s is not a secret but is missing from the redacted diff", kept)
			}
		}
	}
	if !strings.Contains(s, redact.Marker) {
		t.Error("nothing was redacted")
	}
}
