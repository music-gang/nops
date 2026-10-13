package cli

import (
	_ "embed"
	"flag"
	"io"
	"strings"
	"text/template"
)

// The completion scripts are written from the command table and the flags of
// the commands, so a new command or flag gets completion with no extra work.
// A flag takes a file when its usage quotes its value name as `file`, the way
// the flag package reads it; an argument does when its placeholder is <file>.

//go:embed completion.bash.tmpl
var bashTemplate string

//go:embed completion.zsh.tmpl
var zshTemplate string

var completionFuncs = template.FuncMap{
	// quote escapes a text for single quotes in a shell.
	"quote": func(s string) string { return strings.ReplaceAll(s, "'", `'\''`) },
	// spec escapes a text for the description of an option in _arguments,
	// where brackets end it.
	"spec": func(s string) string {
		return strings.NewReplacer(`\`, `\\`, "[", `\[`, "]", `\]`, "'", `'\''`).Replace(s)
	},
}

var completionScripts = map[string]*template.Template{
	"bash": template.Must(template.New("bash").Funcs(completionFuncs).Parse(bashTemplate)),
	"zsh":  template.Must(template.New("zsh").Funcs(completionFuncs).Parse(zshTemplate)),
}

// shellNames are the shells the completion is written for, in the order the
// error names them.
var shellNames = []string{"bash", "zsh"}

// compFlag is a flag of a command, as the scripts offer it.
type compFlag struct {
	Name    string // without the dash
	Help    string
	Metavar string // what its value is
	Value   bool   // takes a value: not a boolean
	File    bool   // the value is a file
}

// compArg is a positional argument.
type compArg struct {
	Name  string
	Pos   int // from 1, as zsh counts them
	File  bool
	Words []string // the values it takes, when there is a fixed list
}

type compChild struct{ Name, Help string }

// compNode is a word of the command line: the root, a group such as "acl
// policy" that takes a subcommand, or a command.
type compNode struct {
	Path     string // the words from the root: "acl policy apply"
	Group    bool
	Children []compChild // of a group
	Flags    []compFlag
	Args     []compArg
}

// argValues are the arguments that take one of a fixed list.
var argValues = map[string][]string{"<shell>": shellNames}

func runCompletion(k *call) error {
	t, ok := completionScripts[k.arg]
	if !ok {
		return usageErrorf("%q is not a shell: %s", k.arg, strings.Join(shellNames, " or "))
	}
	return t.Execute(k.out, completionTree(k.table, k.serve))
}

// completionTree is the root of the command line and every group and command
// under it, parents first. serve, when there is one, is the flags of "nops
// serve".
func completionTree(commands []command, serve *flag.FlagSet) []compNode {
	root := compNode{Group: true, Flags: []compFlag{{Name: "h"}, {Name: "help"}, {Name: "version"}}}
	nodes := []compNode{root}
	at := func(path string) *compNode {
		for i := range nodes {
			if nodes[i].Path == path {
				return &nodes[i]
			}
		}
		return nil
	}
	add := func(path string, n compNode, help string) {
		parent := at(strings.Join(strings.Fields(path)[:len(strings.Fields(path))-1], " "))
		parent.Children = append(parent.Children, compChild{lastWord(path), help})
		n.Path = path
		nodes = append(nodes, n)
	}

	for _, c := range commands {
		words := strings.Fields(c.name)
		for l := 1; l < len(words); l++ {
			if path := strings.Join(words[:l], " "); at(path) == nil {
				add(path, compNode{Group: true}, "")
			}
		}
		var s settings
		n := compNode{Flags: flagsOf(c.flagSet(&call{}, &s, io.Discard))}
		for i, a := range strings.Fields(c.arg) {
			n.Args = append(n.Args, compArg{Name: strings.Trim(a, "<>"), Pos: i + 1, File: a == "<file>", Words: argValues[a]})
		}
		add(c.name, n, c.help)
	}
	if serve != nil {
		add("serve", compNode{Flags: flagsOf(serve)}, "run the server")
	}
	return nodes
}

func lastWord(path string) string {
	w := strings.Fields(path)
	return w[len(w)-1]
}

// flagsOf is the flags in fs, by name (VisitAll sorts them).
func flagsOf(fs *flag.FlagSet) []compFlag {
	var flags []compFlag
	fs.VisitAll(func(f *flag.Flag) {
		metavar, help := flag.UnquoteUsage(f)
		boolean := false
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok {
			boolean = b.IsBoolFlag()
		}
		flags = append(flags, compFlag{Name: f.Name, Help: help, Metavar: metavar, Value: !boolean, File: metavar == "file"})
	})
	return flags
}
