package instance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/aholovko/stave/internal/astinfo"
)

// build derives the manifest row from the source so its hash agrees with the instance.
func build(t *testing.T, src string, siblings ...Row) *Instance {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "src.go", []byte(src), astinfo.ParserMode)
	if err != nil {
		t.Fatalf("fixture does not parse: %v", err)
	}
	rows := append([]Row{{
		Path:     "target.go",
		SHA:      shaOf(src),
		TopDecls: astinfo.TopDeclNames(file),
	}}, siblings...)
	ctx, err := NewPackageContext(rows, true)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := New("target.go", ctx, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

const simpleSrc = "package p\n\nfunc F() int {\n\treturn 1\n}\n"

func TestNewValidatesSourceAndContext(t *testing.T) {
	good := shaOf(simpleSrc)
	const path = "target.go"

	t.Run("nil package context is rejected", func(t *testing.T) {
		if _, err := New(path, nil, []byte(simpleSrc)); err == nil {
			t.Fatal("an instance without package context must be rejected")
		}
	})

	t.Run("source hash must match the distinguished manifest row", func(t *testing.T) {
		ctx, err := NewPackageContext([]Row{{Path: "target.go", SHA: shaOf("something else")}}, true)
		if err != nil {
			t.Fatal(err)
		}
		_, err = New(path, ctx, []byte(simpleSrc))
		if err == nil || !strings.Contains(err.Error(), "does not match its manifest row") {
			t.Fatalf("a source whose bytes do not hash to its row must be rejected, got %v", err)
		}
	})

	t.Run("a distinguished row must exist", func(t *testing.T) {
		ctx, err := NewPackageContext([]Row{{Path: "other.go", SHA: good}}, true)
		if err != nil {
			t.Fatal(err)
		}
		_, err = New(path, ctx, []byte(simpleSrc))
		if err == nil || !strings.Contains(err.Error(), "no distinguished manifest row") {
			t.Fatalf("an instance with no row for its own path must be rejected, got %v", err)
		}
	})

	t.Run("unparsable source is rejected", func(t *testing.T) {
		bad := "package p\n\nfunc F( {\n"
		ctx, err := NewPackageContext([]Row{{Path: "target.go", SHA: shaOf(bad)}}, true)
		if err != nil {
			t.Fatal(err)
		}
		inst, err := New(path, ctx, []byte(bad))
		if err == nil || inst != nil {
			t.Fatalf("a parse failure must return an error and no instance, got %v, %v", inst, err)
		}
		if !strings.Contains(err.Error(), path) {
			t.Fatalf("parse error must identify the source file, got %v", err)
		}
	})

	t.Run("the pinned parser mode preserves comments in the AST", func(t *testing.T) {
		if ParserMode != astinfo.ParserMode {
			t.Fatalf("instance parser mode %v differs from astinfo's %v", ParserMode, astinfo.ParserMode)
		}
		inst := build(t, "package p\n\n// F does a thing.\nfunc F() {}\n")
		if inst.File().Decls[0].(*ast.FuncDecl).Doc == nil {
			t.Fatal("the instance AST lost its doc comment; the parser mode is not ParseComments")
		}
	})

	t.Run("valid source is accepted", func(t *testing.T) {
		inst := build(t, simpleSrc)
		if got := string(inst.Source()); got != simpleSrc {
			t.Fatalf("Source = %q, want %q", got, simpleSrc)
		}
	})
}

func TestPkgFlagsAreWiredIndividually(t *testing.T) {
	base := build(t, simpleSrc)
	for k, name := range PkgFlagNames {
		got, err := base.PkgFlag(name)
		if err != nil {
			t.Fatal(err)
		}
		if got != base.PkgFlags()[k] {
			t.Fatalf("PkgFlag(%q) disagrees with the vector at index %d", name, k)
		}
	}

	// Vary each flag independently to catch constants and swapped vector positions.
	for _, tc := range []struct {
		name    string
		sibling string
		flag    string
	}{
		{"a sibling declaring errors", "package p\n\nvar errors = 1\n", "toplevel_conflict_errors"},
		{"a sibling declaring fmt", "package p\n\nvar fmt = 1\n", "toplevel_conflict_fmt"},
		{"a sibling shadowing int", "package p\n\ntype int struct{}\n", "shadowed_int"},
		{"a sibling shadowing rune", "package p\n\nfunc rune() {}\n", "shadowed_rune"},
		{"a sibling shadowing complex128", "package p\n\nvar complex128 = 1\n", "shadowed_complex128"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "src.go", []byte(tc.sibling), astinfo.ParserMode)
			if err != nil {
				t.Fatal(err)
			}
			inst := build(t, simpleSrc, Row{
				Path: "sibling.go", SHA: shaOf(tc.sibling), TopDecls: astinfo.TopDeclNames(file),
			})
			for _, name := range PkgFlagNames {
				want := name == tc.flag
				got, err := inst.PkgFlag(name)
				if err != nil {
					t.Fatal(err)
				}
				if name == "context_complete" {
					want = true
				}
				if got != want {
					t.Fatalf("flag %q = %v, want %v", name, got, want)
				}
			}
		})
	}

	t.Run("context completeness is carried", func(t *testing.T) {
		ctx, err := NewPackageContext([]Row{
			{Path: "target.go", SHA: shaOf(simpleSrc), TopDecls: []string{"F"}},
			{Path: "huge.go", Status: "oversized"},
		}, false)
		if err != nil {
			t.Fatal(err)
		}
		inst, err := New("target.go", ctx, []byte(simpleSrc))
		if err != nil {
			t.Fatal(err)
		}
		got, err := inst.PkgFlag("context_complete")
		if err != nil {
			t.Fatal(err)
		}
		if got {
			t.Fatal("context_complete must be false when a sibling's declarations are unknown")
		}
	})

	if _, err := base.PkgFlag("no_such_flag"); err == nil {
		t.Fatal("an unknown flag name must fail closed rather than return the zero value")
	}
}

func TestSourceTokensAndFlagsAreCopies(t *testing.T) {
	inst := build(t, simpleSrc)
	src := inst.Source()
	src[0] = 'X'
	if inst.Source()[0] == 'X' {
		t.Fatal("Source handed out the backing array; a caller could make parsed data inconsistent with the source")
	}
	toks := inst.Tokens()
	if len(toks) == 0 {
		t.Fatal("no lexical records derived")
	}
	toks[0].Text = "mutated"
	if inst.Tokens()[0].Text == "mutated" {
		t.Fatal("Tokens handed out the backing array")
	}
	flags := inst.PkgFlags()
	flags[0] = !flags[0]
	if inst.PkgFlags()[0] == flags[0] {
		t.Fatal("PkgFlags handed out the backing array")
	}
}
