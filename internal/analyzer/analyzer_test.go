package analyzer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/aholovko/stave/internal/astinfo"
	"github.com/aholovko/stave/internal/instance"
	"github.com/aholovko/stave/internal/span"
)

func shaOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// build derives package conflicts from the supplied sibling declarations.
func build(t *testing.T, src string, siblingDecls ...string) *instance.Instance {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "src.go", []byte(src), astinfo.ParserMode)
	if err != nil {
		t.Fatalf("fixture does not parse: %v", err)
	}
	rows := []instance.Row{{Path: "target.go", SHA: shaOf(src), TopDecls: astinfo.TopDeclNames(file)}}
	if len(siblingDecls) > 0 {
		rows = append(rows, instance.Row{
			Path: "sibling.go", SHA: shaOf(strings.Join(siblingDecls, ",")), TopDecls: siblingDecls,
		})
	}
	ctx, err := instance.NewPackageContext(rows, true)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := instance.New("target.go", ctx, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

// idx requires a unique match so an expected anchor cannot silently point at the wrong occurrence.
func idx(t *testing.T, src, needle string) int {
	t.Helper()
	first := strings.Index(src, needle)
	if first < 0 {
		t.Fatalf("fixture has no %q", needle)
	}
	if strings.Contains(src[first+1:], needle) {
		t.Fatalf("fixture has more than one %q; the expectation would be ambiguous", needle)
	}
	return first
}

type wantOcc struct {
	class string
	start int
	end   int
}

func occ(class string, start, end int) wantOcc { return wantOcc{class, start, end} }

// anchor checks both coordinates against hand-written token text, catching misplaced spans even when the finding
// count is correct.
func anchor(t *testing.T, src, class, text string) wantOcc {
	t.Helper()
	start := idx(t, src, text)
	return occ(class, start, start+len(text))
}

func check(t *testing.T, src string, want []wantOcc, siblingDecls ...string) {
	t.Helper()
	got, err := Analyze(build(t, src, siblingDecls...))
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	var gotDesc, wantDesc []string
	for _, o := range got {
		gotDesc = append(gotDesc, fmt.Sprintf("%s[%d,%d)=%q", o.Class, o.Span.Start, o.Span.End, src[o.Span.Start:o.Span.End]))
	}
	for _, w := range want {
		wantDesc = append(wantDesc, fmt.Sprintf("%s[%d,%d)=%q", w.class, w.start, w.end, src[w.start:w.end]))
	}
	if strings.Join(gotDesc, "\n") != strings.Join(wantDesc, "\n") {
		t.Fatalf("A_G returned\n  %s\nwant\n  %s", strings.Join(gotDesc, "\n  "), strings.Join(wantDesc, "\n  "))
	}
}

func TestElseAfterReturn(t *testing.T) {
	src := "package p\n\nfunc f(c bool) int {\n\tif c {\n\t\treturn 1\n\t} else {\n\t\treturn 2\n\t}\n}\n"
	check(t, src, []wantOcc{anchor(t, src, "P1", "else")})
}

func TestElseCheckDoesNotFireWithoutATerminatingReturn(t *testing.T) {
	src := "package p\n\nfunc f(c bool) int {\n\tx := 0\n\tif c {\n\t\tx = 1\n\t} else {\n\t\tx = 2\n\t}\n\treturn x\n}\n"
	check(t, src, nil)
}

// The comment contains "else" before the keyword, so a text search would anchor the wrong occurrence.
func TestElseCheckAnchorsOnTheKeywordNotOnTextInAComment(t *testing.T) {
	src := "package p\n\nfunc f(c bool) int {\n\tif c {\n\t\treturn 1\n\t} /* else */ else {\n\t\treturn 2\n\t}\n}\n"
	start := idx(t, src, "else {")
	check(t, src, []wantOcc{occ("P1", start, start+len("else"))})
}

func TestCapitalizedErrorString(t *testing.T) {
	src := "package p\n\nimport \"errors\"\n\nfunc f() error {\n\treturn errors.New(\"Bad thing\")\n}\n"
	check(t, src, []wantOcc{anchor(t, src, "P2", "\"Bad thing\"")})
}

func TestErrorStringCheckExcludesParenthesizedCallees(t *testing.T) {
	src := "package p\n\nimport \"errors\"\n\nvar _ = errors.New(\"Bad.\")\nvar _ = (errors.New)(\"Worse.\")\n"
	check(t, src, []wantOcc{anchor(t, src, "P2", `"Bad."`)})
}

func TestErrorStringEndingInPunctuation(t *testing.T) {
	src := "package p\n\nimport \"fmt\"\n\nfunc f() error {\n\treturn fmt.Errorf(\"wrapping failed.\")\n}\n"
	check(t, src, []wantOcc{anchor(t, src, "P2", "\"wrapping failed.\"")})
}

func TestErrorStringCheckDoesNotFireOnInteriorPunctuation(t *testing.T) {
	src := "package p\n\nimport \"fmt\"\n\nfunc f() error {\n\treturn fmt.Errorf(\"bad thing: %w\", nil)\n}\n"
	check(t, src, nil)
}

// Literal coordinates fix the anchor convention independently of substring lookup: the string literal includes
// both quotes.
func TestErrorStringAnchorIncludesTheQuotes(t *testing.T) {
	// The literal "Bad" starts at byte 47.
	src := "package p\n\nimport \"errors\"\n\nvar E = errors.New(\"Bad\")\n"
	if got := src[47:52]; got != `"Bad"` {
		t.Fatalf("the fixture layout changed: src[47:52] = %q, want %q", got, `"Bad"`)
	}
	check(t, src, []wantOcc{occ("P2", 47, 52)})
}

func TestErrorStringCheckDoesNotFireOnIdiomaticStrings(t *testing.T) {
	for _, msg := range []string{"bad thing", "HTTP request failed", "URL is empty"} {
		t.Run(msg, func(t *testing.T) {
			src := "package p\n\nimport \"errors\"\n\nfunc f() error {\n\treturn errors.New(\"" + msg + "\")\n}\n"
			check(t, src, nil)
		})
	}
}

func TestErrorStringCheckRequiresUnshadowedImportsAndLiteralMessages(t *testing.T) {
	body := "func f() error {\n\treturn errors.New(\"Bad thing\")\n}\n"
	t.Run("an aliased import is out of scope", func(t *testing.T) {
		check(t, "package p\n\nimport errors \"errors\"\n\n"+body, nil)
	})
	t.Run("a local declaration of the base disables the class", func(t *testing.T) {
		src := "package p\n\nimport \"errors\"\n\nfunc g() {\n\terrors := 1\n\t_ = errors\n}\n\n" + body
		check(t, src, nil)
	})
	t.Run("a package-block conflict in a sibling disables the class", func(t *testing.T) {
		check(t, "package p\n\nimport \"errors\"\n\n"+body, nil, "errors")
	})
	t.Run("a non-literal message is out of scope", func(t *testing.T) {
		src := "package p\n\nimport \"errors\"\n\nvar m = \"Bad thing\"\n\nfunc f() error {\n\treturn errors.New(m)\n}\n"
		check(t, src, nil)
	})
}

// An unrelated import alias does not shadow errors and must not suppress the error-string check.
func TestErrorStringCheckIgnoresAnUnrelatedImportAlias(t *testing.T) {
	src := "package p\n\nimport (\n\t\"errors\"\n\n\talias \"os\"\n)\n\nvar _ = alias.Args\n\nfunc f() error {\n\treturn errors.New(\"Bad thing\")\n}\n"
	check(t, src, []wantOcc{anchor(t, src, "P2", "\"Bad thing\"")})
}

func TestDeclaredNameWithAnUnderscore(t *testing.T) {
	src := "package p\n\nfunc f() {\n\tmy_var := 1\n\t_ = my_var\n}\n"
	start := idx(t, src, "my_var :=")
	check(t, src, []wantOcc{occ("P3", start, start+len("my_var"))})
}

func TestDeclaredNameCheckExcludesReceiverTypeParameters(t *testing.T) {
	src := "package p\n\nvar raw_input int\n\ntype T[X any] int\n\nfunc (T[a_b]) M() {}\n"
	check(t, src, []wantOcc{anchor(t, src, "P3", "raw_input")})
}

func TestDeclaredNameCheckAllowsTheBlankIdentifierAndIdiomaticNames(t *testing.T) {
	src := "package p\n\nfunc f() {\n\tfor _, v := range []int{1} {\n\t\t_ = v\n\t}\n\tID := 1\n\t_ = ID\n}\n"
	check(t, src, nil)
}

func TestAnalyzerReportsOnlyTheThreeSupportedChecks(t *testing.T) {
	check(t, mixedFixture, []wantOcc{
		anchor(t, mixedFixture, "P2", `"Bad thing"`),
		anchor(t, mixedFixture, "P1", "else"),
		anchor(t, mixedFixture, "P3", "snake_case"),
	})
}

// Duplicate identities would silently lose findings in set comparisons.
func TestOccurrenceIdentitiesAreInjective(t *testing.T) {
	for _, src := range allFixtures {
		got, err := Analyze(build(t, src))
		if err != nil {
			t.Fatalf("Analyze(%q): %v", firstLine(src), err)
		}
		seen := map[span.Occurrence]bool{}
		for _, o := range got {
			if seen[o] {
				t.Fatalf("fixture %q produced duplicate identity %s", firstLine(src), o)
			}
			seen[o] = true
		}
	}
}

// Sorted output keeps comparisons deterministic; all anchors must also use valid source coordinates and class
// shapes.
func TestAnchorsAreWellFormedAndOrdered(t *testing.T) {
	for _, src := range allFixtures {
		got, err := Analyze(build(t, src))
		if err != nil {
			t.Fatalf("Analyze(%q): %v", firstLine(src), err)
		}
		for i, o := range got {
			if o.Span.Start < 0 || o.Span.End > len(src) || o.Span.Start > o.Span.End {
				t.Fatalf("%s has anchor %s outside the %d-byte source", o.Class, o.Span, len(src))
			}
			if o.Span.Zero() {
				t.Fatalf("%s has a zero-length anchor; every supported check points at source text", o.Class)
			}
			if i > 0 && got[i-1].Span.Start > o.Span.Start {
				t.Fatalf("result is not sorted: %s follows %s", o, got[i-1])
			}
		}
	}
}

func TestAnalyzeFailsClosedOnANilInstance(t *testing.T) {
	if _, err := Analyze(nil); err == nil {
		t.Fatal("a nil instance must be rejected rather than analysed as empty")
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// The mixed fixture includes all three supported checks alongside import layout,
// documentation, redundant types, receiver naming and package-name repetition.
const mixedFixture = `package parser

import (
	"os"
	"errors"
)
import "fmt"

type ParserNode struct{}

// Wrong builds one.
func NewNode() *ParserNode {
	var n int = 1
	_ = n
	return nil
}

func (this *ParserNode) Fail(c bool) error {
	if c {
		return errors.New("Bad thing")
	} else {
		return nil
	}
}

var snake_case = fmt.Sprint(os.Getenv("HOME"))
`

// allFixtures covers every supported check so the shared invariants are checked across the full analyzer.
var allFixtures = []string{
	"package p\n\nfunc f(c bool) int {\n\tif c {\n\t\treturn 1\n\t} else {\n\t\treturn 2\n\t}\n}\n",
	"package p\n\nimport \"errors\"\n\nvar E = errors.New(\"Bad\")\n",
	"package p\n\nimport \"fmt\"\n\nfunc f() error {\n\treturn fmt.Errorf(\"wrapping failed.\")\n}\n",
	"package p\n\nfunc f() {\n\tmy_var := 1\n\t_ = my_var\n}\n",
	mixedFixture,
}

// Require every supported class so the shared invariant checks cannot silently miss one.
func TestEveryRetainedClassIsExercisedByAFixture(t *testing.T) {
	seen := map[string]bool{}
	for _, src := range allFixtures {
		got, err := Analyze(build(t, src))
		if err != nil {
			t.Fatalf("Analyze(%q): %v", firstLine(src), err)
		}
		for _, o := range got {
			seen[o.Class] = true
		}
	}
	for _, r := range rules {
		if !seen[r.class] {
			t.Errorf("no fixture in allFixtures produces a %s occurrence; its invariants are vacuous", r.class)
		}
	}
}

// No current rule emits duplicate identities, so this exercises the guard directly.
func TestCheckInjectiveRejectsADuplicateIdentity(t *testing.T) {
	one, err := span.NewOccurrence("P2", span.Span{Start: 10, End: 14}, span.AffNone)
	if err != nil {
		t.Fatal(err)
	}
	other, err := span.NewOccurrence("P2", span.Span{Start: 20, End: 24}, span.AffNone)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkInjective([]span.Occurrence{one, other}); err != nil {
		t.Fatalf("distinct identities must be accepted: %v", err)
	}
	if err := checkInjective([]span.Occurrence{one, other, one}); err == nil {
		t.Fatal("a repeated identity must be rejected")
	}

	// The class distinguishes identities even when the spans match.
	overlapping, err := span.NewOccurrence("P3", span.Span{Start: 10, End: 14}, span.AffNone)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkInjective([]span.Occurrence{one, overlapping}); err != nil {
		t.Fatalf("different classes at overlapping positions are distinct identities: %v", err)
	}
}
