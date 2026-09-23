package hooks

import (
	"errors"
	"fmt"
	"regexp"
	"sort"

	"github.com/hashicorp/nomad/api"
)

// Dispatch meta keys nops offers to a hook. A hook receives only the ones it
// declares in meta_required/meta_optional (see DispatchMeta).
const (
	MetaDeploymentID = "nops_deployment_id"
	MetaJobID        = "nops_job_id"
	MetaCommit       = "nops_commit"
	MetaPhase        = "nops_phase"
	// MetaImagePrefix is followed by the sanitized task name.
	MetaImagePrefix = "nops_image_"
)

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// BuildMeta returns every dispatch meta nops can offer for a deployment of
// target: the fixed keys plus nops_image_<task> for each docker task. The task
// name is sanitized (every non-alphanumeric character becomes "_") so it is a
// valid meta key and a usable ${NOMAD_META_...} name. Two tasks that sanitize
// to the same name are fine if they use the same image and an error otherwise.
func BuildMeta(deploymentID, commit, phase string, target *api.Job) (map[string]string, error) {
	if target == nil || target.ID == nil {
		return nil, errors.New("build hook meta: no target job")
	}
	meta := map[string]string{
		MetaDeploymentID: deploymentID,
		MetaJobID:        *target.ID,
		MetaCommit:       commit,
		MetaPhase:        phase,
	}
	owner := map[string]string{} // meta key -> "group/task" that set it
	for _, g := range target.TaskGroups {
		if g == nil {
			continue
		}
		groupName := ""
		if g.Name != nil {
			groupName = *g.Name
		}
		for _, t := range g.Tasks {
			if t == nil || t.Driver != "docker" {
				continue
			}
			image, ok := t.Config["image"].(string)
			if !ok || image == "" {
				continue
			}
			key := MetaImagePrefix + nonAlnum.ReplaceAllString(t.Name, "_")
			where := groupName + "/" + t.Name
			if prev, dup := meta[key]; dup && prev != image {
				return nil, fmt.Errorf("build hook meta: tasks %s and %s both map to %s but use different images (%q, %q)",
					owner[key], where, key, prev, image)
			}
			meta[key] = image
			owner[key] = where
		}
	}
	return meta, nil
}

// DispatchMeta keeps, out of all, only the meta the parameterized parent job
// declares in meta_required or meta_optional: Nomad rejects the dispatch
// otherwise. It fails if a required key is not in all, naming the key.
func DispatchMeta(parent *api.Job, all map[string]string) (map[string]string, error) {
	if parent == nil || parent.ParameterizedJob == nil {
		return nil, errors.New("hook job is not parameterized")
	}
	out := map[string]string{}
	var missing []string
	for _, k := range parent.ParameterizedJob.MetaRequired {
		v, ok := all[k]
		if !ok {
			missing = append(missing, k)
			continue
		}
		out[k] = v
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("hook job requires meta %v that nops cannot provide (available: %v)", missing, sortedKeys(all))
	}
	for _, k := range parent.ParameterizedJob.MetaOptional {
		if v, ok := all[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
