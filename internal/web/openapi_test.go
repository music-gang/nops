package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
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
		"ACLPolicy":        reflect.TypeOf(apiACLPolicy{}),
		"ACLPolicyBody":    reflect.TypeOf(policyBody{}),
		"ACLToken":         reflect.TypeOf(apiACLToken{}),
		"NewACLToken":      reflect.TypeOf(apiNewACLToken{}),
		"ACLTokenBody":     reflect.TypeOf(tokenBody{}),
		"Change":           reflect.TypeOf(apiChange{}),
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
	row := regexp.MustCompile("(?m)^\\| `((?:GET|POST|PUT|DELETE) /api/[^`]*)` \\|")
	var rows []string
	for _, m := range row.FindAllSubmatch(page, -1) {
		rows = append(rows, string(m[1]))
	}
	sameSet(t, "the rows of docs/api.md and the operations of openapi.json", rows, spec.operations())
}

// oasSchemas are the JSON Schemas of OpenAPI 3.1 that validate a description,
// as the OpenAPI Initiative publishes them: the tests run without a network.
//
//go:embed testdata/oas3.1/*.json
var oasSchemas embed.FS

// specURL is where the tests place openapi.json for the schema compiler.
const specURL = "https://nops.test/openapi.json"

// newCompiler makes a compiler that knows openapi.json, so a JSON pointer into
// it names a schema and its $refs resolve.
func newCompiler(doc []byte) (*jsonschema.Compiler, error) {
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return nil, err
	}
	if err := c.AddResource(specURL, parsed); err != nil {
		return nil, err
	}
	return c, nil
}

// validateOpenAPI checks a description against the OpenAPI 3.1 schema.
func validateOpenAPI(doc []byte) error {
	c := jsonschema.NewCompiler()
	for name, url := range map[string]string{
		"schema.json":       "https://spec.openapis.org/oas/3.1/schema/2022-10-07",
		"schema-base.json":  "https://spec.openapis.org/oas/3.1/schema-base/2022-10-07",
		"dialect-base.json": "https://spec.openapis.org/oas/3.1/dialect/base",
		"meta-base.json":    "https://spec.openapis.org/oas/3.1/meta/base",
	} {
		raw, err := oasSchemas.ReadFile("testdata/oas3.1/" + name)
		if err != nil {
			return err
		}
		parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := c.AddResource(url, parsed); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	schema, err := c.Compile("https://spec.openapis.org/oas/3.1/schema-base/2022-10-07")
	if err != nil {
		return err
	}
	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return err
	}
	return schema.Validate(parsed)
}

// The file is a valid OpenAPI 3.1 document, and the check notices one that is
// not.
func TestOpenAPIIsValid(t *testing.T) {
	if err := validateOpenAPI(openAPIFile); err != nil {
		t.Fatalf("openapi.json is not valid OpenAPI 3.1:\n%v", err)
	}

	for name, broken := range map[string]func(spec map[string]any){
		"responses is not an object": func(spec map[string]any) {
			spec["paths"].(map[string]any)["/api/jobs"].(map[string]any)["get"].(map[string]any)["responses"] = "none"
		},
		"a path does not start with a slash": func(spec map[string]any) {
			paths := spec["paths"].(map[string]any)
			paths["api/jobs"] = paths["/api/jobs"]
			delete(paths, "/api/jobs")
		},
		"a schema has a keyword of the wrong type": func(spec map[string]any) {
			spec["components"].(map[string]any)["schemas"].(map[string]any)["Error"].(map[string]any)["required"] = "error"
		},
		"there is no info": func(spec map[string]any) { delete(spec, "info") },
	} {
		t.Run(name, func(t *testing.T) {
			var spec map[string]any
			if err := json.Unmarshal(openAPIFile, &spec); err != nil {
				t.Fatal(err)
			}
			broken(spec)
			doc, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			if validateOpenAPI(doc) == nil {
				t.Error("the check accepts it")
			}
		})
	}
}

// answerChecker checks an answer of the API against openapi.json.
type answerChecker struct {
	raw      map[string]any
	paths    []pathTemplate
	compiler *jsonschema.Compiler
	schemas  map[string]*jsonschema.Schema
}

// pathTemplate is a path of the description with {parameters}.
type pathTemplate struct {
	path string
	re   *regexp.Regexp
}

func newAnswerChecker(doc []byte) (*answerChecker, error) {
	var raw map[string]any
	if err := json.Unmarshal(doc, &raw); err != nil {
		return nil, err
	}
	c, err := newCompiler(doc)
	if err != nil {
		return nil, err
	}
	a := &answerChecker{raw: raw, compiler: c, schemas: map[string]*jsonschema.Schema{}}
	param := regexp.MustCompile(`\\\{[^/]*?\\\}`)
	for path := range raw["paths"].(map[string]any) {
		re := param.ReplaceAllString(regexp.QuoteMeta(path), `[^/]+`)
		a.paths = append(a.paths, pathTemplate{path, regexp.MustCompile("^" + re + "$")})
	}
	return a, nil
}

var answers = sync.OnceValues(func() (*answerChecker, error) { return newAnswerChecker(openAPIFile) })

// pointer is the JSON pointer of a path inside openapi.json, as a fragment.
func pointer(tokens ...string) string {
	var parts []string
	for _, tok := range tokens {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~", "~0"), "/", "~1")
		parts = append(parts, url.PathEscape(tok))
	}
	return "#/" + strings.Join(parts, "/")
}

// check compares an answer with the operation it is for: its status is one
// the operation lists, and its body is the one of that status, an object that
// matches the schema or nothing. A request to a path the description does not
// have is not checked: TestOpenAPIDescribesEveryRoute covers the paths.
func (a *answerChecker) check(method, target string, rec *httptest.ResponseRecorder) error {
	reqURL, err := url.Parse(target)
	if err != nil {
		return err
	}
	var path string
	for _, p := range a.paths {
		if p.re.MatchString(reqURL.Path) {
			path = p.path
			break
		}
	}
	if path == "" {
		return nil
	}
	item, _ := a.raw["paths"].(map[string]any)[path].(map[string]any)
	operation, ok := item[strings.ToLower(method)].(map[string]any)
	if !ok {
		return nil
	}

	status := fmt.Sprint(rec.Code)
	responses, _ := operation["responses"].(map[string]any)
	response, ok := responses[status].(map[string]any)
	if !ok {
		return fmt.Errorf("%s %s answered %d: the description lists %v", method, path, rec.Code, keys(responses))
	}
	where := []string{"paths", path, strings.ToLower(method), "responses", status}
	if ref, ok := response["$ref"].(string); ok {
		where = strings.Split(strings.TrimPrefix(ref, "#/"), "/")
		response, _ = a.raw["components"].(map[string]any)["responses"].(map[string]any)[where[len(where)-1]].(map[string]any)
	}

	content, _ := response["content"].(map[string]any)
	if len(content) == 0 {
		if rec.Body.Len() != 0 {
			return fmt.Errorf("%s %s answered %d with a body, the description has none: %s", method, path, rec.Code, rec.Body)
		}
		return nil
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		return fmt.Errorf("%s %s answered %d with Content-Type %q, the description has application/json", method, path, rec.Code, ct)
	}
	key := strings.Join(where, "/")
	schema, ok := a.schemas[key]
	if !ok {
		schema, err = a.compiler.Compile(specURL + pointer(append(where, "content", "application/json", "schema")...))
		if err != nil {
			return fmt.Errorf("the schema of %s %s %d: %w", method, path, rec.Code, err)
		}
		a.schemas[key] = schema
	}
	body, err := jsonschema.UnmarshalJSON(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		return fmt.Errorf("%s %s answered %d with a body that is not JSON: %w", method, path, rec.Code, err)
	}
	if err := schema.Validate(body); err != nil {
		return fmt.Errorf("%s %s answered %d with a body the description does not match: %w\n%s", method, path, rec.Code, err, rec.Body)
	}
	return nil
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The check of an answer fails on a status the operation does not list, on a
// body where the description has none, and on one that does not match.
func TestOpenAPIAnswerCheck(t *testing.T) {
	a, err := answers()
	if err != nil {
		t.Fatal(err)
	}
	answer := func(code int, contentType, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		if contentType != "" {
			rec.Header().Set("Content-Type", contentType)
		}
		rec.WriteHeader(code)
		rec.WriteString(body)
		return rec
	}
	const j = "application/json"
	for _, tc := range []struct {
		name           string
		method, target string
		rec            *httptest.ResponseRecorder
		ok             bool
	}{
		{"a listed status with its body", "GET", "/api/jobs", answer(200, j, `{"jobs":[]}`), true},
		{"a shared response", "GET", "/api/jobs/default/web", answer(404, j, `{"error":"no"}`), true},
		{"no body where there is none", "POST", "/api/deployments/d1/reject", answer(204, "", ""), true},
		{"a path the description does not have", "GET", "/api/nothing", answer(404, j, `{}`), true},
		{"a status the operation does not list", "GET", "/api/jobs", answer(409, j, `{"error":"x"}`), false},
		{"a body where there is none", "POST", "/api/deployments/d1/reject", answer(204, j, `{}`), false},
		{"no body where there is one", "GET", "/api/jobs", answer(200, j, ``), false},
		{"a field that is missing", "GET", "/api/jobs", answer(200, j, `{}`), false},
		{"a field of another type", "GET", "/api/jobs", answer(200, j, `{"jobs":{}}`), false},
		{"a field inside a $ref", "GET", "/api/jobs", answer(200, j, `{"jobs":[{"namespace":"default"}]}`), false},
		{"a date that is not one", "GET", "/api/jobs", answer(200, j, `{"jobs":[{"namespace":"d","job":"w","policy":"auto","sync":"sync","drift":false,"hold":{"kind":"k","reason":"r","since":"yesterday"}}]}`), false},
		{"a content type that is not JSON", "GET", "/api/jobs", answer(200, "text/plain", `{"jobs":[]}`), false},
		{"an error without its message", "GET", "/api/jobs/default/web", answer(404, j, `{}`), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := a.check(tc.method, tc.target, tc.rec)
			if tc.ok && err != nil {
				t.Errorf("rejected: %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("accepted")
			}
		})
	}
}
