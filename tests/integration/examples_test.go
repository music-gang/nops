//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"

	"github.com/music-gang/nops/internal/meta"
)

// TestExamplesParse keeps examples/ from rotting: every job and hook in it
// must still parse with Nomad's own parser (with its <name>.vars.hcl) and
// carry valid nops_* meta, every hook a job declares must exist in the same
// directory as a hook, and nothing may need more than Nomad: services use
// Nomad's own discovery (provider = "nomad") and no file mentions Consul,
// since a job with a Consul service is not even placed on a cluster without
// it. It does not run the examples: their docker images are not pulled here.
func TestExamplesParse(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()

	dirs, err := filepath.Glob(filepath.Join("..", "..", "examples", "*"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no example directories found: %v", err)
	}
	for _, dir := range dirs {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		t.Run(filepath.Base(dir), func(t *testing.T) {
			files, err := filepath.Glob(filepath.Join(dir, "*.nomad.hcl"))
			if err != nil || len(files) == 0 {
				t.Fatalf("no *.nomad.hcl in %s: %v", dir, err)
			}

			cfgs := map[string]meta.Config{} // job ID → its meta
			for _, path := range files {
				src, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var vars string
				varsPath := strings.TrimSuffix(path, ".nomad.hcl") + ".vars.hcl"
				if b, err := os.ReadFile(varsPath); err == nil {
					vars = string(b)
				}
				if strings.Contains(strings.ToLower(string(src)), "consul") {
					t.Errorf("%s mentions Consul: examples must work with Nomad alone (provider = \"nomad\", nomadService in a template)", filepath.Base(path))
				}
				job, err := c.ParseHCL(ctx, string(src), vars)
				if err != nil {
					t.Fatalf("%s does not parse: %v", filepath.Base(path), err)
				}
				for _, svc := range services(job) {
					if svc.Provider != "nomad" {
						t.Errorf("%s: service %q has provider %q, want \"nomad\"", filepath.Base(path), svc.Name, svc.Provider)
					}
				}
				cfg := meta.Parse(job.Meta)
				for _, issue := range cfg.Issues {
					t.Errorf("%s: %s: %s", filepath.Base(path), issue.Severity, issue)
				}
				cfgs[*job.ID] = cfg
			}

			for id, cfg := range cfgs {
				for _, h := range []*meta.Hook{cfg.PreHook, cfg.PostHook} {
					if h == nil {
						continue
					}
					target, ok := cfgs[h.JobID]
					if !ok {
						t.Errorf("job %s declares hook %q, which is not in %s", id, h.JobID, dir)
					} else if !target.IsHook {
						t.Errorf("job %s declares %q as a hook, but it has no nops_role = \"hook\"", id, h.JobID)
					}
				}
			}
		})
	}
}

// services lists every service of a job, at group and at task level.
func services(job *api.Job) []*api.Service {
	var out []*api.Service
	for _, tg := range job.TaskGroups {
		out = append(out, tg.Services...)
		for _, task := range tg.Tasks {
			out = append(out, task.Services...)
		}
	}
	return out
}
