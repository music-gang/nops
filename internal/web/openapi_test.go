package web

import (
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// openAPI is the part of openapi.json the tests read.
type openAPI struct {
	OpenAPI string                                `json:"openapi"`
	Paths   map[string]map[string]json.RawMessage `json:"paths"`
	Schemas map[string]openAPISchema              `json:"-"`
}

type openAPISchema struct {
	Ref         string                   `json:"$ref"`
	Description string                   `json:"description"`
	AllOf       []openAPISchema          `json:"allOf"`
	Required    []string                 `json:"required"`
	Properties  map[string]openAPISchema `json:"properties"`
}

func loadOpenAPI(t *testing.T) (spec openAPI, raw map[string]any) {
	t.Helper()
	if err := json.Unmarshal(openAPIFile, &spec); err != nil {
		t.Fatalf("openapi.json is not JSON: %v", err)
	}
	if err := json.Unmarshal(openAPIFile, &raw); err != nil {
		t.Fatal(err)
	}
	var comps struct {
		Components struct {
			Schemas map[string]openAPISchema `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openAPIFile, &comps); err != nil {
		t.Fatal(err)
	}
	spec.Schemas = comps.Components.Schemas
	return spec, raw
}

// operations is every "METHOD /path" the description holds.
func (o openAPI) operations() []string {
	var ops []string
	for path, item := range o.Paths {
		for method := range item {
			ops = append(ops, strings.ToUpper(method)+" "+path)
		}
	}
	sort.Strings(ops)
	return ops
}

func sameSet(t *testing.T, what string, got, want []string) {
	t.Helper()
	got, want = append([]string{}, got...), append([]string{}, want...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n got  %q\n want %q", what, got, want)
	}
}

// A route without an entry in the description, or an entry without a route,
// fails the build.
func TestOpenAPIDescribesEveryRoute(t *testing.T) {
	spec, _ := loadOpenAPI(t)
	if !strings.HasPrefix(spec.OpenAPI, "3.1") {
		t.Errorf("openapi = %q, want 3.1.x", spec.OpenAPI)
	}
	s := &server{trigger: func() {}} // with Fetch now: the one route that is not always there
	var routes []string
	for _, e := range s.apiEndpoints() {
		routes = append(routes, e.pattern)
	}
	sameSet(t, "the routes and the operations of openapi.json", routes, spec.operations())
}

// Every $ref points at something the description holds.
func TestOpenAPIRefsResolve(t *testing.T) {
	_, raw := loadOpenAPI(t)
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if ref, ok := v["$ref"].(string); ok {
				var node any = raw
				for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
					m, _ := node.(map[string]any)
					if node = m[part]; node == nil {
						t.Errorf("%s does not resolve", ref)
						break
					}
				}
			}
			for _, c := range v {
				walk(c)
			}
		case []any:
			for _, c := range v {
				walk(c)
			}
		}
	}
	walk(raw)
}

// jsonFields is the JSON names of a struct's fields, the embedded ones
// included, and which of them are always there (no omitempty).
func jsonFields(typ reflect.Type) (all, required []string) {
	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.Anonymous {
			a, r := jsonFields(f.Type)
			all, required = append(all, a...), append(required, r...)
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		all = append(all, name)
		if !strings.Contains(opts, "omitempty") {
			required = append(required, name)
		}
	}
	return all, required
}

// flatten merges a schema with the ones it is made of.
func (o openAPI) flatten(s openAPISchema) (props, required []string) {
	if s.Ref != "" {
		return o.flatten(o.Schemas[strings.TrimPrefix(s.Ref, "#/components/schemas/")])
	}
	for _, part := range s.AllOf {
		p, r := o.flatten(part)
		props, required = append(props, p...), append(required, r...)
	}
	for name := range s.Properties {
		props = append(props, name)
	}
	return props, append(required, s.Required...)
}

// A schema has the fields of the struct the API answers or reads with, and the
// same ones are always there.
func TestOpenAPISchemasMatchTheAnswers(t *testing.T) {
	spec, _ := loadOpenAPI(t)
	for name, typ := range map[string]reflect.Type{
		"Hold":             reflect.TypeOf(apiHold{}),
		"Deployment":       reflect.TypeOf(apiDeployment{}),
		"Job":              reflect.TypeOf(apiJob{}),
		"Issue":            reflect.TypeOf(apiIssue{}),
		"JobDetail":        reflect.TypeOf(apiJobDetail{}),
		"Event":            reflect.TypeOf(apiEvent{}),
		"HookRun":          reflect.TypeOf(apiHookRun{}),
		"DeploymentDetail": reflect.TypeOf(apiDeploymentDetail{}),
		"SpecHash":         reflect.TypeOf(specBody{}),
		"Pause":            reflect.TypeOf(pauseBody{}),
	} {
		t.Run(name, func(t *testing.T) {
			schema, ok := spec.Schemas[name]
			if !ok {
				t.Fatalf("openapi.json has no schema %s", name)
			}
			props, required := spec.flatten(schema)
			all, always := jsonFields(typ)
			sameSet(t, "properties", props, all)
			sameSet(t, "required", required, always)
		})
	}

	t.Run("sync", func(t *testing.T) {
		desc := spec.Schemas["Job"].Properties["sync"].Description
		for _, k := range syncOrder {
			if !strings.Contains(desc, string(k)) {
				t.Errorf("the sync state %q is not in the description of Job.sync", k)
			}
		}
	})
}

// The description holds no secret, but it is behind the token like the rest of
// /api/: whoever has none learns nothing about which paths exist.
func TestOpenAPIIsServedBehindTheToken(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")

	rec := ts.api("GET", "/api/openapi.json", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("without a token: status %d, want 401", rec.Code)
	}
	apiErrorOf(t, rec)

	rec = ts.api("GET", "/api/openapi.json", apiToken(t, ts, "alice", time.Time{}), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("with a token: status %d, want 200: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if rec.Body.String() != string(openAPIFile) || !json.Valid(rec.Body.Bytes()) {
		t.Error("the body is not openapi.json")
	}
}

// The table of docs/api.md lists the operations of the description, no more
// and no fewer.
func TestAPIDocsTableMatchesOpenAPI(t *testing.T) {
	spec, _ := loadOpenAPI(t)
	page, err := os.ReadFile("../../docs/api.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("(?m)^\\| `((?:GET|POST) /api/[^`]*)` \\|")
	var rows []string
	for _, m := range row.FindAllSubmatch(page, -1) {
		rows = append(rows, string(m[1]))
	}
	sameSet(t, "the rows of docs/api.md and the operations of openapi.json", rows, spec.operations())
}
