// Package docs holds the tests that read the Markdown pages of the repository:
// docs/development.md#what-needs-which-tests says what they guard.
package docs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
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

// agentWords are words that only belong in an instruction to an AI agent.
// "agent" and "session" alone are not among them: in the docs they are a
// Nomad agent and a dashboard login.
var agentWords = regexp.MustCompile(`(?i)\bclaude\b|\bassistants?\b|\bAI\b|\bLLMs?\b|\bprompts?\b|\bin the session\b`)

// TestDocsSpeakToPeople fails when a Markdown page other than CLAUDE.md, the
// file for agents, talks about or to an AI agent: the docs describe Nops and
// how anyone contributes, and what only an agent needs lives in CLAUDE.md.
// It reads words, not tone: a written rule alone kept being broken.
func TestDocsSpeakToPeople(t *testing.T) {
	for _, page := range markdownPages(t) {
		name := filepath.ToSlash(strings.TrimPrefix(page, ".."+string(filepath.Separator)))
		if name == "CLAUDE.md" || strings.HasPrefix(name, "docs/archive/") {
			continue
		}
		b, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		for n, line := range strings.Split(string(b), "\n") {
			// A product, not an assistant: one of the notification targets.
			line = strings.ReplaceAll(line, "Home Assistant", "")
			if w := agentWords.FindString(line); w != "" {
				t.Errorf("%s:%d says %q: what only an agent needs goes in CLAUDE.md, not in the docs", name, n+1, w)
			}
		}
	}
}

// numberedBold is the first bold phrase of a numbered list item.
var numberedBold = regexp.MustCompile(`^\d+\.\s+\*\*(.+?)\*\*`)

// invariantTitles returns the first bold phrase of each numbered item under
// the "## Invariants" heading of a page, without a trailing period.
func invariantTitles(b []byte) []string {
	var titles []string
	in := false
	for _, line := range proseLines(b) {
		if strings.HasPrefix(line, "## ") {
			in = strings.HasPrefix(line, "## Invariants")
			continue
		}
		if m := numberedBold.FindStringSubmatch(line); in && m != nil {
			titles = append(titles, strings.TrimSuffix(m[1], "."))
		}
	}
	return titles
}

// TestInvariantTitlesMatch keeps the short list of invariants in CLAUDE.md
// the same as the one philosophy.md explains, in the same order, so the copy
// cannot drift.
func TestInvariantTitlesMatch(t *testing.T) {
	philosophy, err := os.ReadFile("philosophy.md")
	if err != nil {
		t.Fatal(err)
	}
	claude, err := os.ReadFile("../CLAUDE.md")
	if err != nil {
		t.Fatal(err)
	}
	want, got := invariantTitles(philosophy), invariantTitles(claude)
	if len(want) == 0 {
		t.Fatal("philosophy.md: no numbered bold item under \"## Invariants\"")
	}
	if !slices.Equal(got, want) {
		t.Errorf("the invariants of CLAUDE.md differ from philosophy.md:\n  CLAUDE.md:     %q\n  philosophy.md: %q", got, want)
	}
}
