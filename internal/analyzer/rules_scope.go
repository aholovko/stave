package analyzer

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"
	"unicode"
)

// checkErrorStrings checks error-message literals passed through unaliased errors or fmt imports, with complete
// package context and no conflicting declarations. A local declaration of either name disables its check file-wide,
// avoiding scope tracking while preventing false reports on shadowed names.
func (s *scan) checkErrorStrings() {
	complete, err := s.inst.PkgFlag("context_complete")
	if err != nil {
		s.err = err
		return
	}
	if !complete {
		return // Unknown sibling declarations could shadow errors or fmt.
	}
	shadowed := s.declaredNamesExcludingImports()

	for _, base := range []string{"errors", "fmt"} {
		conflict, err := s.inst.PkgFlag("toplevel_conflict_" + base)
		if err != nil {
			s.err = err
			return
		}
		if conflict || shadowed[base] || !s.importsUnaliased(base) {
			continue
		}
		s.scanErrorCalls(base)
	}
}

func (s *scan) scanErrorCalls(base string) {
	want := "New"
	if base == "fmt" {
		want = "Errorf"
	}
	ast.Inspect(s.file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != want {
			return true
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok || x.Name != base {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true // not a direct string literal: out of scope
		}
		msg, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		if offendingErrorString(msg) {
			s.emitNode("P2", lit)
		}
		return true
	})
}

// offendingErrorString allows multi-rune messages with no lowercase second rune, so initialisms such as HTTP and
// URL are not rejected for capitalization.
func offendingErrorString(msg string) bool {
	runes := []rune(msg)
	if len(runes) == 0 {
		return false
	}
	if unicode.IsUpper(runes[0]) && (len(runes) == 1 || unicode.IsLower(runes[1])) {
		return true
	}
	return strings.ContainsRune(".:!\n", runes[len(runes)-1])
}

// importsUnaliased rejects every explicit alias, even one that repeats the package name: the error-string check
// requires the unaliased import form.
func (s *scan) importsUnaliased(path string) bool {
	for _, sp := range s.file.Imports {
		if importKey(sp) == path {
			return sp.Name == nil
		}
	}
	return false
}

// importKey falls back to raw text for an invalid literal so distinct invalid paths do not compare equal.
func importKey(sp *ast.ImportSpec) string {
	if sp.Path == nil {
		return ""
	}
	if unquoted, err := strconv.Unquote(sp.Path.Value); err == nil {
		return unquoted
	}
	return sp.Path.Value
}

func (s *scan) declaredNamesIncludingImports() map[string]bool {
	out := map[string]bool{}
	for _, id := range s.declaredIdents() {
		out[id.Name] = true
	}
	return out
}

// declaredNamesExcludingImports leaves alias checks to importsUnaliased, which checkErrorStrings applies separately.
func (s *scan) declaredNamesExcludingImports() map[string]bool {
	out := s.declaredNamesIncludingImports()
	for _, sp := range s.file.Imports {
		if sp.Name != nil {
			delete(out, sp.Name.Name)
		}
	}
	return out
}
