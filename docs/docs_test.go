// Package docs holds the one test that reads every Markdown page of the
// repository: docs/development.md#doc-audit says what it guards.
package docs

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEveryTestNamedInTheDocsExists fails when a test named in any .md file
// does not exist as a function in the repository, so renaming or deleting a
// test forces the page that cites it to change. A name ending in * ("TestE2ERetry*")
// stands for a group and needs at least one test with that prefix. It proves the
// test exists, not that it asserts what the page says.
func TestEveryTestNamedInTheDocsExists(t *testing.T) {
	root := ".."
	mention := regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]*\*?`)
	funcDecl := regexp.MustCompile(`(?m)^func (Test[A-Z][A-Za-z0-9_]*)\(`)

	var pages, sources []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".md":
			pages = append(pages, path)
		case ".go":
			sources = append(sources, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) == 0 || len(sources) == 0 {
		t.Fatalf("found %d pages and %d Go files under %s: is the test run from docs/?", len(pages), len(sources), root)
	}

	tests := map[string]bool{}
	for _, src := range sources {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range funcDecl.FindAllStringSubmatch(string(b), -1) {
			tests[m[1]] = true
		}
	}
	names := make([]string, 0, len(tests))
	for name := range tests {
		names = append(names, name)
	}
	sort.Strings(names)

	exists := func(name string) bool {
		if prefix, group := strings.CutSuffix(name, "*"); group {
			for _, n := range names {
				if strings.HasPrefix(n, prefix) {
					return true
				}
			}
			return false
		}
		return tests[name]
	}
	for _, page := range pages {
		b, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, name := range mention.FindAllString(string(b), -1) {
			if seen[name] {
				continue
			}
			seen[name] = true
			if !exists(name) {
				t.Errorf("%s names %s, which is not a test in the repository", filepath.ToSlash(strings.TrimPrefix(page, root+string(filepath.Separator))), name)
			}
		}
	}
}
