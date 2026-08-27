package auth

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/arloliu/profgate/internal/config"
)

func TestDisabled(t *testing.T) {
	cfg := &config.Config{Auth: config.AuthConfig{Mode: "disabled", AnonymousRealm: "everyone"}}
	r := httptest.NewRequestWithContext(context.Background(), "GET", "/v1/x", nil)
	p, err := Disabled{}.Authenticate(context.Background(), r, cfg)
	if err != nil {
		t.Fatalf("Authenticate error = %v", err)
	}
	if p.Name != "anonymous" || p.Realm != "everyone" {
		t.Fatalf("Principal = %+v, want anonymous in everyone", p)
	}
}

func TestChallenge(t *testing.T) {
	cases := map[string]string{
		"disabled": "",
		"basic":    `Basic realm="profgate"`,
		"oidc":     `Bearer realm="profgate"`,
	}
	for mode, want := range cases {
		t.Run(mode, func(t *testing.T) {
			if got := Challenge(mode); got != want {
				t.Fatalf("Challenge(%q) = %q, want %q", mode, got, want)
			}
		})
	}
}

func TestFailureError(t *testing.T) {
	var err error = &Failure{Status: 401, Reason: ReasonMissing}
	var f *Failure
	if !errors.As(err, &f) || f.Reason != ReasonMissing {
		t.Fatalf("errors.As failed for %v", err)
	}
	if !strings.Contains(err.Error(), ReasonMissing) {
		t.Fatalf("Error() = %q, want it to name the reason", err.Error())
	}
}

// TestReasons pins the closed set: Reasons() lists every Reason* constant
// once, in table order, and nothing in the package builds a Failure from a
// string literal, so the reasons the package can emit are exactly Reasons().
func TestReasons(t *testing.T) {
	got := Reasons()
	if len(got) != 23 {
		t.Fatalf("Reasons() has %d entries, want 23", len(got))
	}
	seen := map[string]bool{}
	for _, r := range got {
		if seen[r] {
			t.Fatalf("Reasons() lists %q twice", r)
		}
		seen[r] = true
	}

	consts, literals := reasonSources(t)
	if !slices.Equal(got, consts) {
		t.Fatalf("Reasons() = %v\nwant the Reason* constants in source order %v", got, consts)
	}
	if len(literals) != 0 {
		t.Fatalf("Failure literals built from a string rather than a Reason* constant: %v", literals)
	}
}

// reasonSources parses the package's non-test sources and returns the values
// of every Reason* constant in declaration order, and the file:line of every
// `Reason: "..."` composite literal field.
func reasonSources(t *testing.T) ([]string, []string) {
	t.Helper()
	fset := token.NewFileSet()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var consts, literals []string
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name) //nolint:gosec // the package's own sources, from Glob
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), `Reason: "`) {
			literals = append(literals, name)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if !strings.HasPrefix(id.Name, "Reason") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s is not a string literal", id.Name)
					}
					consts = append(consts, strings.Trim(lit.Value, `"`))
				}
			}
		}
	}

	return consts, literals
}
