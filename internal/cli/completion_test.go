package cli

import (
	"bytes"
	"context"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/music-gang/nops/internal/config"
)

// completion prints the script of shell, as "nops completion" does.
func completion(t *testing.T, shell string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	args := []string{"completion", shell}
	if code := Run(context.Background(), args, func(string) string { return "" }, strings.NewReader(""), &out, &errOut, config.FlagSet(io.Discard)); code != 0 || errOut.Len() != 0 {
		t.Fatalf("nops completion %s: exit %d, stderr %q", shell, code, errOut.String())
	}
	return out.String()
}

func TestCompletionNamesEveryCommandAndFlag(t *testing.T) {
	for _, shell := range shellNames {
		script := completion(t, shell)
		for _, c := range commands {
			if !strings.Contains(script, `"`+c.name+`")`) {
				t.Errorf("%s: no word for nops %s", shell, c.name)
			}
			var s settings
			c.flagSet(&call{}, &s, io.Discard).VisitAll(func(f *flag.Flag) {
				if !strings.Contains(script, "-"+f.Name) {
					t.Errorf("%s: nops %s: no flag -%s", shell, c.name, f.Name)
				}
			})
		}
		config.FlagSet(io.Discard).VisitAll(func(f *flag.Flag) {
			if !strings.Contains(script, "-"+f.Name) {
				t.Errorf("%s: nops serve: no flag -%s", shell, f.Name)
			}
		})
	}
}

func TestCompletionRunsOffline(t *testing.T) {
	// No NOPS_ADDR and no token: it must not look for either.
	for _, shell := range shellNames {
		if code, out, errOut := run(t, nil, "", "completion", shell); code != 0 || errOut != "" || out == "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q, want the script", shell, code, out, errOut)
		}
	}
}

func TestCompletionOfAnotherShellIsAUsageError(t *testing.T) {
	for _, args := range [][]string{{"completion", "fish"}, {"completion"}, {"completion", "bash", "zsh"}, {"completion", "-addr", "http://x", "bash"}} {
		code, out, _ := run(t, nil, "", args...)
		if code != exitUsage || out != "" {
			t.Errorf("%v: exit %d, stdout %q, want a usage error and nothing", args, code, out)
		}
	}
	if _, _, errOut := run(t, nil, "", "completion", "fish"); !strings.Contains(errOut, "bash or zsh") {
		t.Errorf("stderr %q, want it to name bash and zsh", errOut)
	}
}

// bashReplies runs the bash script the way the shell does on TAB, with
// words as the command line, the last one being the one under the cursor.
func bashReplies(t *testing.T, script, dir string, words ...string) []string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	file := filepath.Join(t.TempDir(), "nops.bash")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, "-c", `source "$1"; shift; COMP_WORDS=("$@"); COMP_CWORD=$(($# - 1)); _nops; printf '%s\n' "${COMPREPLY[@]}"`, "bash", file)
	cmd.Args = append(cmd.Args, words...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", words, err, out)
	}
	// The shell sorts the candidates: compgen -f lists a directory as the system does.
	replies := strings.Fields(string(out))
	slices.Sort(replies)
	return replies
}

func TestBashCompletion(t *testing.T) {
	script := completion(t, "bash")
	dir := t.TempDir()
	for _, f := range []string{"a.hcl", "b.hcl"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files := []string{"a.hcl", "b.hcl"}
	for _, tc := range []struct {
		words []string
		want  []string // nil: nothing
	}{
		{[]string{"nops", ""}, []string{"jobs", "acl", "completion", "secret", "serve"}},
		{[]string{"nops", "ac"}, []string{"acl"}},
		{[]string{"nops", "acl", ""}, []string{"binding-rule", "policy", "token"}},
		{[]string{"nops", "acl", "token", "cr"}, []string{"create"}},
		{[]string{"nops", "acl", "token", "delete", ""}, nil},
		{[]string{"nops", "-"}, []string{"-h", "-help", "-version"}},
		{[]string{"nops", "approve", "-"}, []string{"-addr", "-json", "-token-file", "-yes"}},
		{[]string{"nops", "approve", "-y"}, []string{"-yes"}},
		{[]string{"nops", "secret", "generate", "-"}, nil},
		{[]string{"nops", "pause", "web", "-"}, []string{"-addr", "-all", "-json", "-namespace", "-reason", "-token-file"}},
		{[]string{"nops", "acl", "token", "create", "-p"}, []string{"-policy"}},
		{[]string{"nops", "completion", ""}, []string{"bash", "zsh"}},
		{[]string{"nops", "completion", "b"}, []string{"bash"}},
		// Files: an argument that is a <file>, and the value of a flag that takes one.
		{[]string{"nops", "backup", ""}, files},
		{[]string{"nops", "backup", "a"}, []string{"a.hcl"}},
		{[]string{"nops", "acl", "policy", "apply", "name", ""}, files},
		{[]string{"nops", "acl", "policy", "apply", "-description", "x", "name", ""}, files},
		{[]string{"nops", "jobs", "-token-file", ""}, files},
		{[]string{"nops", "serve", "-db-path", ""}, files},
		// No files where there is none: a name, or the value of a flag that is not a file.
		{[]string{"nops", "acl", "policy", "apply", ""}, nil},
		{[]string{"nops", "acl", "policy", "apply", "-token-file", "a.hcl", ""}, nil},
		{[]string{"nops", "jobs", "-addr", ""}, nil},
		{[]string{"nops", "job", "-namespace", "x", "-"}, []string{"-addr", "-json", "-namespace", "-token-file"}},
		{[]string{"nops", "serve", "-nomad-tok"}, []string{"-nomad-token-file"}},
		{[]string{"nops", "serve", "-nomad-token-file", "b"}, []string{"b.hcl"}},
		{[]string{"nops", "frobnicate", ""}, nil},
		{[]string{"nops", "acl", "frobnicate", ""}, nil},
	} {
		got := bashReplies(t, script, dir, tc.words...)
		if tc.want == nil {
			if len(got) != 0 {
				t.Errorf("%q: %q, want nothing", tc.words, got)
			}
			continue
		}
		// The words of the root are many: check it holds the ones it should.
		if len(tc.words) == 2 && tc.words[1] == "" {
			for _, w := range tc.want {
				if !slices.Contains(got, w) {
					t.Errorf("%q: %q has no %q", tc.words, got, w)
				}
			}
			continue
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%q: %q, want %q", tc.words, got, tc.want)
		}
	}
}

func TestZshCompletionLoads(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("no zsh")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "nops.zsh")
	if err := os.WriteFile(file, []byte(completion(t, "zsh")), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(zsh, "-f", "-c", `autoload -Uz compinit && compinit -u -d "$1/zcompdump" && source "$2" && print -r -- $_comps[nops]`, "zsh", dir, file).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "_nops" {
		t.Errorf("zsh: %v, output %q, want the function _nops to complete nops", err, out)
	}
}
