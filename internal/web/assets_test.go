package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/music-gang/nops/internal/store"
)

func TestAssetVersionFollowsTheContent(t *testing.T) {
	a := fstest.MapFS{"app.css": {Data: []byte("body { color: red }")}}
	b := fstest.MapFS{"app.css": {Data: []byte("body { color: blue }")}}

	va, err := assetVersion(a, "app.css")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := assetVersion(a, "app.css"); again != va {
		t.Errorf("the same bytes gave %q then %q: the version must be stable, or the cache is useless", va, again)
	}
	if vb, _ := assetVersion(b, "app.css"); vb == va {
		t.Errorf("changed bytes kept the version %q: a browser would keep the old file", va)
	}
	if !regexp.MustCompile(`^[0-9a-f]{10}$`).MatchString(va) {
		t.Errorf("version %q is not 10 hex digits", va)
	}

	if _, err := assetVersion(a, "missing.css"); err == nil {
		t.Error("a missing file must be an error")
	}
}

func TestAssetIsTheEmbeddedFilesAddress(t *testing.T) {
	got, err := asset("app.css")
	if err != nil {
		t.Fatal(err)
	}
	b, err := fs.ReadFile(staticFiles, "static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if want := "/static/app.css?v=" + hex.EncodeToString(sum[:])[:10]; got != want {
		t.Errorf("asset(app.css) = %q, want %q", got, want)
	}
	if _, err := asset("nope.css"); err == nil {
		t.Error("asset of a missing file must be an error, so a page fails loud instead of linking a 404")
	}
}

// Every page that loads the stylesheet links it with its version, the login
// page included: it is the first one a browser with a stale cache sees.
func TestPagesLinkVersionedAssets(t *testing.T) {
	css, err := asset("app.css")
	if err != nil {
		t.Fatal(err)
	}
	js, err := asset("htmx.min.js")
	if err != nil {
		t.Fatal(err)
	}

	d := sampleDeployment()
	d.State = store.StateCompleted
	ts := newTestServer(t, &fakeStore{deployment: d}, &fakeEngine{}, "")
	for _, p := range []string{"/", "/jobs", "/history", "/deployments/d1"} {
		page := ts.get(p)
		mustContain(t, page, `href="`+css+`"`, `src="`+js+`"`)
		mustNotContain(t, page, `"/static/app.css"`, `"/static/htmx.min.js"`)
	}

	ba := newBasicApp(t, "alice:"+bcryptHash(t, "s3cret")+"\n")
	login := ba.do("GET", "/auth/login", nil, nil).Body.String()
	if !strings.Contains(login, `href="`+css+`"`) || strings.Contains(login, `"/static/app.css"`) {
		t.Errorf("the login page does not link the versioned stylesheet %s", css)
	}
}
