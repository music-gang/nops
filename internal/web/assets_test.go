package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image/png"
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

// Every page offers the favicon (16 and 32 px) and the touch icon, the login
// page included, and the header shows the logo next to the name. All of them go through asset(), so a
// changed file has a new address.
func TestPagesLinkTheIconsAndTheLogo(t *testing.T) {
	addr := func(name string) string {
		t.Helper()
		a, err := asset(name)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	icons := []string{
		`<link rel="icon" type="image/png" sizes="32x32" href="` + addr("favicon-32x32.png") + `">`,
		`<link rel="icon" type="image/png" sizes="16x16" href="` + addr("favicon-16x16.png") + `">`,
		`<link rel="apple-touch-icon" sizes="180x180" href="` + addr("apple-touch-icon.png") + `">`,
	}

	d := sampleDeployment()
	d.State = store.StateCompleted
	ts := newTestServer(t, &fakeStore{deployment: d}, &fakeEngine{}, "")
	for _, p := range []string{"/", "/jobs", "/history", "/deployments/d1"} {
		page := ts.get(p)
		mustContain(t, page, icons...)
		mustContain(t, page, "<title>Nops</title>")
		mustContain(t, page, `<img class="app-header__logo" src="`+addr("logo-96x96.png")+`" alt="Nops">`)
	}

	ba := newBasicApp(t, "alice:"+bcryptHash(t, "s3cret")+"\n")
	login := ba.do("GET", "/auth/login", nil, nil).Body.String()
	mustContain(t, login, icons...)
	mustContain(t, login,
		`<img class="login__logo" src="`+addr("logo-192x192.png")+`" alt="" width="96" height="96">`,
		`<h1 class="login__title">Sign in to Nops</h1>`)
}

// The files the pages link exist, are served as images, and each PNG has the
// size its name (and its link) announces.
func TestIconsAreServedWithTheirType(t *testing.T) {
	ts := newTestServer(t, &fakeStore{}, &fakeEngine{}, "")
	sizes := map[string]int{
		"favicon-16x16.png":    16,
		"favicon-32x32.png":    32,
		"apple-touch-icon.png": 180,
		"logo-96x96.png":       96,
		"logo-192x192.png":     192,
	}
	for name, size := range sizes {
		rec := ts.do("GET", "/static/"+name, nil)
		if rec.Code != 200 {
			t.Errorf("GET /static/%s: status %d", name, rec.Code)
			continue
		}
		if got := rec.Header().Get("Content-Type"); got != "image/png" {
			t.Errorf("GET /static/%s: Content-Type %q, want image/png", name, got)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if cfg.Width != size || cfg.Height != size {
			t.Errorf("%s is %dx%d, want %dx%d", name, cfg.Width, cfg.Height, size, size)
		}
	}
}
