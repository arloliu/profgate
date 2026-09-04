package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
)

// helpNode is one command line the binary answers a help argument for:
// the words that name it,
// the grammar line printed after "usage: profgate ",
// whether it prints the global flags,
// the subverbs it lists when it is a group,
// the flags it registers when it is a leaf,
// and the flags at this node that take a separated value, which is what the page walk steps over.
type helpNode struct {
	path       []string
	grammar    string
	globals    bool
	children   []string
	flags      func(fs *flag.FlagSet)
	valueFlags map[string]bool
}

// group reports whether the node takes a word after it rather than running.
func (n helpNode) group() bool { return len(n.children) > 0 }

// helpNodes derives the tree from the verb table alone,
// so a verb added later gets its help without a second edit:
// the bare binary, one node per leaf, and one per prefix of a leaf's path that is not itself a leaf.
func helpNodes(verbs []verb) []helpNode {
	names := make([]string, 0, len(verbs))
	for _, v := range verbs {
		names = append(names, v.name)
	}
	nodes := []helpNode{{globals: true, children: names}}
	for _, v := range verbs {
		nodes = append(nodes, v.helpNodes()...)
	}
	return append(nodes, operatorHelpNodes()...)
}

// The grammar line of each operator command line a run function names,
// printed on that command line's page and below the cause of its usage errors.
const (
	serveGrammar          = "serve --config <path>"
	configValidateGrammar = "config validate --config <path>"
	versionGrammar        = "version"
)

// configGrammar and authGrammar are the two operator groups' lines,
// each derived from the one child beside it,
// so a group's page and the usage error its own dispatch prints carry the same text.
func configGrammar() string { return groupGrammar([]string{"config"}, []string{"validate"}) }

func authGrammar() string { return groupGrammar([]string{"auth"}, []string{"hash"}) }

// operatorHelpNodes is the operator half of the tree, which the verb table does not carry.
// Every one of these command lines takes no global flag, so no page of theirs prints one:
// the dispatcher hands an operator name to its own function before the global flag set exists.
// Each group takes its line from the children beside it, as every client group does,
// so --config is named on the config validate page and on no other.
// collector has no node, because it names no implementation yet and so has no grammar to print;
// a bare word that names no child of the root prints the bare binary's page, which is the answer it gets.
func operatorHelpNodes() []helpNode {
	config := func(fs *flag.FlagSet) { configPath(fs) }
	takesConfig := valueFlags(config)

	return []helpNode{
		{path: []string{"serve"}, grammar: serveGrammar, flags: config, valueFlags: takesConfig},
		{path: []string{"version"}, grammar: versionGrammar},
		{
			path:     []string{"config"},
			grammar:  configGrammar(),
			children: []string{"validate"},
			// The union of the group's leaves, which is what the page walk steps over while standing here.
			valueFlags: takesConfig,
		},
		{
			path: []string{"config", "validate"}, grammar: configValidateGrammar,
			flags: config, valueFlags: takesConfig,
		},
		{
			path:    []string{"auth"},
			grammar: authGrammar(), children: []string{"hash"},
		},
		{path: []string{"auth", "hash"}, grammar: "auth hash"},
	}
}

// operatorHelp prints the page an operator command line names, and reports whether it printed one.
// The dispatch answers help before either group function runs;
// this is the second road to the same page,
// for a group that reaches no flag set of its own and so has nothing to return flag's help error.
func operatorHelp(stdout io.Writer, name string, args []string) bool {
	scanned, help := findHelp(args)
	if !help {
		return false
	}
	verbs := clientVerbs()
	printHelp(stdout, verbs, helpTarget(helpNodes(verbs), append([]string{name}, scanned...)))

	return true
}

// operatorFlagError answers what an operator flag set refused.
// A help argument prints that command line's own page on stdout and exits 0;
// anything else prints the cause and the command line's grammar on stderr and exits 2.
// The set writes nothing itself, so flag's own usage block reaches neither stream.
func operatorFlagError(stdout, stderr io.Writer, path []string, grammar string, err error) int {
	if errors.Is(err, flag.ErrHelp) {
		verbs := clientVerbs()
		printHelp(stdout, verbs, helpTarget(helpNodes(verbs), path))

		return exitOK
	}
	_, _ = fmt.Fprintf(stderr, "profgate: %v\nusage: profgate %s\n", err, grammar)

	return 2
}

// helpNodes is one verb's half of the tree:
// a group for every proper prefix of a leaf's path that is not itself a leaf, then a node per leaf.
// A group carries no flags, because a flag belongs to the leaf that registers it;
// it carries the union of its leaves' value-taking flags all the same,
// because the walk has to step over a value while standing there.
func (v verb) helpNodes() []helpNode {
	isLeaf := map[string]bool{}
	for _, l := range v.leaves {
		isLeaf[strings.Join(leafPath(v, &l), " ")] = true
	}
	var groups []helpNode
	at := map[string]int{}
	for _, l := range v.leaves {
		path := leafPath(v, &l)
		values := valueFlags(l.flags)
		for i := 1; i < len(path); i++ {
			key := strings.Join(path[:i], " ")
			if isLeaf[key] {
				continue
			}
			j, ok := at[key]
			if !ok {
				groups = append(groups, helpNode{path: path[:i:i], globals: true, valueFlags: map[string]bool{}})
				j = len(groups) - 1
				at[key] = j
			}
			g := &groups[j]
			if !slices.Contains(g.children, path[i]) {
				g.children = append(g.children, path[i])
			}
			for name := range values {
				g.valueFlags[name] = true
			}
		}
	}
	nodes := make([]helpNode, 0, len(groups)+len(v.leaves))
	for _, g := range groups {
		g.grammar = groupGrammar(g.path, g.children)
		nodes = append(nodes, g)
	}
	for _, l := range v.leaves {
		nodes = append(nodes, helpNode{
			path:       leafPath(v, &l),
			grammar:    l.grammar,
			globals:    true,
			flags:      l.flags,
			valueFlags: valueFlags(l.flags),
		})
	}
	return nodes
}

// leafPath is the words that name one of a verb's command lines, or the verb alone where no leaf is given.
func leafPath(v verb, l *leaf) []string {
	path := []string{v.name}
	if l != nil {
		path = append(path, strings.Fields(l.words)...)
	}
	return path
}

// groupGrammar is a group's line:
// its own path, then the one word the reader has to add next, spelled as the alternatives it takes.
func groupGrammar(path, children []string) string {
	return strings.Join(path, " ") + " " + strings.Join(children, "|")
}

// findNode is the node whose path is exactly these words, or nil.
func findNode(nodes []helpNode, path []string) *helpNode {
	for i := range nodes {
		if slices.Equal(nodes[i].path, path) {
			return &nodes[i]
		}
	}
	return nil
}

// valueFlags is the set of flags a registration declares that take a separated value:
// every flag whose value does not report itself a boolean,
// which is the question flag itself asks before it consumes the next argument.
func valueFlags(register func(fs *flag.FlagSet)) map[string]bool {
	names := map[string]bool{}
	if register == nil {
		return names
	}
	fs := flag.NewFlagSet("help", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	register(fs)
	fs.VisitAll(func(f *flag.Flag) {
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			return
		}
		names[f.Name] = true
	})
	return names
}

// valueGlobals is the global half of that set, which every node carries:
// nine of the eleven global flags take a value, and --verbose and --token-stdin do not.
func valueGlobals() map[string]bool {
	return valueFlags((&globals{}).register)
}

// flagName is the text after an argument's one or two leading dashes, cut at the first "=",
// which is how flag reads a flag's name.
// It is empty for an argument that names no flag.
func flagName(arg string) string {
	if len(arg) < 2 || arg[0] != '-' {
		return ""
	}
	name := strings.TrimPrefix(arg[1:], "-")
	name, _, _ = strings.Cut(name, "=")
	return name
}

// findHelp reports whether a help argument is on the line,
// and returns the arguments before the bare "--" that ends the search.
// The search stops there because flag stops there,
// and a line the two read differently is a line nobody can predict.
// It steps over no value, because it asks only whether a help argument is on the line:
// that is what makes "--file --help" help rather than a file named --help.
func findHelp(args []string) (scanned []string, help bool) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], help
		}
		if name := flagName(arg); name == "h" || name == "help" {
			help = true
		}
	}
	return args, help
}

// helpTarget is the page a command line names:
// the deepest verb and subverb the walk reaches,
// and the bare binary's node for a line that names none.
// A word beginning with "-" is skipped,
// and where its name is a value-taking flag of the node the walk stands on and the word carries no "=",
// the word after it is skipped with it.
// A bare word while the walk stands on a group must name one of that group's children,
// and the walk stops at the first leaf,
// because every word after a leaf is that leaf's own positional or flag value.
func helpTarget(nodes []helpNode, args []string) helpNode {
	root := nodes[0]
	node := root
	globalValues := valueGlobals()
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			name := flagName(arg)
			if !strings.Contains(arg, "=") && (globalValues[name] || node.valueFlags[name]) {
				i++
			}
			continue
		}
		next := findNode(nodes, append(slices.Clone(node.path), arg))
		if next == nil {
			return root
		}
		node = *next
		if !node.group() {
			return node
		}
	}
	return node
}

// printHelp writes one node's page:
// the usage line, the flags the command registers, the subverbs a group takes,
// and the global flags a client command line accepts, with one blank line between blocks.
// The two flag blocks are printed from flag sets built for printing alone,
// so each description stays in one place, the flag's own registration.
func printHelp(w io.Writer, verbs []verb, n helpNode) {
	if len(n.path) == 0 {
		_, _ = fmt.Fprintln(w, usageLine(verbs))
	} else {
		_, _ = fmt.Fprintf(w, "usage: profgate %s\n", n.grammar)
	}
	if n.flags != nil {
		_, _ = fmt.Fprint(w, "\nFlags:\n")
		printFlags(w, n.flags)
	}
	if len(n.path) > 0 && n.group() {
		_, _ = fmt.Fprint(w, "\nSubcommands:\n")
		for _, child := range n.children {
			_, _ = fmt.Fprintf(w, "  %s\n", child)
		}
	}
	if n.globals {
		_, _ = fmt.Fprint(w, "\nGlobal flags:\n")
		printFlags(w, (&globals{}).register)
	}
}

// printFlags prints one registration's defaults to w.
func printFlags(w io.Writer, register func(fs *flag.FlagSet)) {
	fs := flag.NewFlagSet("help", flag.ContinueOnError)
	fs.SetOutput(w)
	register(fs)
	fs.PrintDefaults()
}
