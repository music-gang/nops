// Package docs holds the tests that read the Markdown pages of the repository:
// docs/development.md#doc-audit says what they guard.
package docs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
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

// TestREADMENamesTheNomadVersionCITestsWith fails when the README's Nomad
// compatibility line names a version other than the one the integration job
// of ci.yml installs, so the version is written by hand in exactly two places
// and they cannot drift (docs/development.md#nomad-version).
func TestREADMENamesTheNomadVersionCITestsWith(t *testing.T) {
	ci, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*NOMAD_VERSION:\s*"?(\d+\.\d+\.\d+)"?\s*$`).FindSubmatch(ci)
	if m == nil {
		t.Fatal("NOMAD_VERSION not found in .github/workflows/ci.yml")
	}
	want := string(m[1])

	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	found := regexp.MustCompile(`\*\*Nomad (\d+\.\d+\.\d+)\*\*`).FindAllSubmatch(readme, -1)
	if len(found) != 1 {
		t.Fatalf("README.md has %d mentions of the form **Nomad X.Y.Z**, want exactly one (the compatibility section)", len(found))
	}
	if got := string(found[0][1]); got != want {
		t.Errorf("README.md says Nomad %s, ci.yml tests against %s", got, want)
	}
}

// markdownPages returns every .md file of the repository, relative to docs/.
func markdownPages(t *testing.T) []string {
	t.Helper()
	var pages []string
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
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
		if filepath.Ext(path) == ".md" {
			pages = append(pages, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) == 0 {
		t.Fatal("found no .md file under ..: is the test run from docs/?")
	}
	return pages
}

var (
	fence        = regexp.MustCompile("^\\s*(```|~~~)")
	inlineCode   = regexp.MustCompile("`[^`]*`")
	linkTarget   = regexp.MustCompile(`\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	headingLine  = regexp.MustCompile(`^#{1,6}\s+(.*?)\s*#*\s*$`)
	headingLinks = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
)

// proseLines returns the lines of a Markdown page outside fenced code blocks.
func proseLines(b []byte) []string {
	var lines []string
	inFence := false
	for _, line := range strings.Split(string(b), "\n") {
		if fence.MatchString(line) {
			inFence = !inFence
			continue
		}
		if !inFence {
			lines = append(lines, line)
		}
	}
	return lines
}

// anchors returns the anchors GitHub generates for the headings of a page:
// lowercase, spaces to hyphens, everything but letters, digits, hyphens and
// underscores dropped, and -1, -2... on repeats.
func anchors(b []byte) map[string]bool {
	out := map[string]bool{}
	count := map[string]int{}
	for _, line := range proseLines(b) {
		m := headingLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		text := headingLinks.ReplaceAllString(m[1], "$1")
		var slug strings.Builder
		for _, r := range strings.ToLower(text) {
			switch {
			case unicode.IsLetter(r), unicode.IsDigit(r), r == '-', r == '_':
				slug.WriteRune(r)
			case r == ' ':
				slug.WriteRune('-')
			}
		}
		s := slug.String()
		if n := count[s]; n > 0 {
			out[fmt.Sprintf("%s-%d", s, n)] = true
		} else {
			out[s] = true
		}
		count[s]++
	}
	return out
}

// TestRelativeLinksResolve fails when a relative link in any .md file points
// to a file that does not exist, or to an anchor that is not a heading of the
// page it names, so moving or renaming a page or a heading forces every link
// to it to change.
func TestRelativeLinksResolve(t *testing.T) {
	pages := markdownPages(t)
	headings := map[string]map[string]bool{}
	headingsOf := func(path string) (map[string]bool, error) {
		if h, ok := headings[path]; ok {
			return h, nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		headings[path] = anchors(b)
		return headings[path], nil
	}

	for _, page := range pages {
		b, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.ToSlash(strings.TrimPrefix(page, ".."+string(filepath.Separator)))
		for _, line := range proseLines(b) {
			for _, m := range linkTarget.FindAllStringSubmatch(inlineCode.ReplaceAllString(line, ""), -1) {
				target := m[1]
				if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
					continue
				}
				path, frag, _ := strings.Cut(target, "#")
				file := page
				if path != "" {
					file = filepath.Join(filepath.Dir(page), filepath.FromSlash(path))
					if _, err := os.Stat(file); err != nil {
						t.Errorf("%s links to %s, which does not exist", name, target)
						continue
					}
				}
				if frag == "" || filepath.Ext(file) != ".md" {
					continue
				}
				h, err := headingsOf(file)
				if err != nil {
					t.Fatal(err)
				}
				if !h[frag] {
					t.Errorf("%s links to %s, which is not a heading of that page", name, target)
				}
			}
		}
	}
}

// changeKinds are the sections of a version in CHANGELOG.md, in the order
// Keep a Changelog gives them.
var changeKinds = []string{"Added", "Changed", "Deprecated", "Removed", "Fixed", "Security"}

// checkChangelog returns what is wrong with a CHANGELOG.md: "## [Unreleased]"
// first, then "## [X.Y.Z] - YYYY-MM-DD" sections from the newest down; inside
// each, only the sections of changeKinds, in that order, each with at least
// one "- " line, and no line outside them.
func checkChangelog(b []byte) []string {
	var problems []string
	release := regexp.MustCompile(`^## \[(\d+)\.(\d+)\.(\d+)\] - (\d{4}-\d{2}-\d{2})$`)
	var version, prev [3]int
	versions, kindAt, bullets := 0, -1, 0
	kind := ""
	endKind := func() {
		if kind != "" && bullets == 0 {
			problems = append(problems, fmt.Sprintf("%q in %s has no \"- \" line", kind, versionName(versions, version)))
		}
		kind, bullets = "", 0
	}
	for n, line := range proseLines(b) {
		switch {
		case strings.HasPrefix(line, "## "):
			endKind()
			kindAt = -1
			if versions == 0 {
				if line != "## [Unreleased]" {
					problems = append(problems, fmt.Sprintf("line %d: the first version heading is %q, want \"## [Unreleased]\"", n+1, line))
				}
				versions++
				continue
			}
			m := release.FindStringSubmatch(line)
			if m == nil {
				problems = append(problems, fmt.Sprintf("line %d: %q is not \"## [X.Y.Z] - YYYY-MM-DD\"", n+1, line))
				continue
			}
			for i := range 3 {
				version[i], _ = strconv.Atoi(m[i+1])
			}
			if _, err := time.Parse(time.DateOnly, m[4]); err != nil {
				problems = append(problems, fmt.Sprintf("line %d: %q is not a date", n+1, m[4]))
			}
			if versions > 1 && !older(version, prev) {
				problems = append(problems, fmt.Sprintf("line %d: %d.%d.%d comes after %d.%d.%d, want the newest first", n+1, version[0], version[1], version[2], prev[0], prev[1], prev[2]))
			}
			prev = version
			versions++
		case strings.HasPrefix(line, "### "):
			endKind()
			kind = strings.TrimPrefix(line, "### ")
			at := slices.Index(changeKinds, kind)
			switch {
			case versions == 0:
				problems = append(problems, fmt.Sprintf("line %d: %q comes before \"## [Unreleased]\"", n+1, line))
			case at < 0:
				problems = append(problems, fmt.Sprintf("line %d: %q is not one of %s", n+1, line, strings.Join(changeKinds, ", ")))
			case at <= kindAt:
				problems = append(problems, fmt.Sprintf("line %d: %q comes after a section it should precede (order: %s)", n+1, line, strings.Join(changeKinds, ", ")))
			}
			kindAt = max(kindAt, at)
		case strings.HasPrefix(line, "- "):
			if versions > 0 && kind == "" {
				problems = append(problems, fmt.Sprintf("line %d: a change outside a section (### %s)", n+1, strings.Join(changeKinds, ", ### ")))
			}
			bullets++
		}
	}
	endKind()
	if versions == 0 {
		problems = append(problems, "no \"## [Unreleased]\" heading")
	}
	return problems
}

// versionName names the version a line of CHANGELOG.md belongs to.
func versionName(versions int, v [3]int) string {
	if versions <= 1 {
		return "Unreleased"
	}
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2])
}

// older reports whether version a is lower than version b.
func older(a, b [3]int) bool {
	for i := range 3 {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// TestChangelogFormat keeps CHANGELOG.md in the shape scripts/changelog.sh
// reads and a person expects from Keep a Changelog
// (docs/development.md#changelog).
func TestChangelogFormat(t *testing.T) {
	b, err := os.ReadFile("../CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range checkChangelog(b) {
		t.Errorf("CHANGELOG.md: %s", p)
	}
}

// TestChangelogFormatCatches proves checkChangelog finds each mistake it is
// meant to find, since the real CHANGELOG.md only shows that a good file passes.
func TestChangelogFormatCatches(t *testing.T) {
	good := `# Changelog

## [Unreleased]

### Added

- A page.

## [0.5.0] - 2026-10-01

### Changed

- **Breaking:** renamed an option.

### Fixed

- A crash.

## [0.4.1] - 2026-09-30

### Fixed

- A bug.
`
	if p := checkChangelog([]byte(good)); len(p) > 0 {
		t.Fatalf("a good changelog fails: %v", p)
	}
	bad := map[string]string{
		"no Unreleased":   strings.Replace(good, "## [Unreleased]\n\n### Added\n\n- A page.\n\n", "", 1),
		"no date":         strings.Replace(good, "## [0.5.0] - 2026-10-01", "## [0.5.0]", 1),
		"bad date":        strings.Replace(good, "2026-10-01", "2026-13-01", 1),
		"oldest first":    strings.Replace(good, "[0.5.0]", "[0.3.0]", 1),
		"unknown section": strings.Replace(good, "### Added", "### New", 1),
		"out of order": strings.Replace(good,
			"### Changed\n\n- **Breaking:** renamed an option.\n\n### Fixed\n\n- A crash.",
			"### Fixed\n\n- A crash.\n\n### Changed\n\n- **Breaking:** renamed an option.", 1),
		"repeated section":       strings.Replace(good, "### Fixed\n\n- A crash.", "### Changed\n\n- A crash.", 1),
		"empty section":          strings.Replace(good, "### Added\n\n- A page.", "### Added", 1),
		"line outside a section": strings.Replace(good, "### Added\n\n- A page.", "- A page.", 1),
	}
	for name, text := range bad {
		if len(checkChangelog([]byte(text))) == 0 {
			t.Errorf("%s: checkChangelog found nothing wrong", name)
		}
	}
}
