package structural

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// Mutate parsed ASTs to exercise integrity checks unreachable through valid parser
// output. These cases do not broaden the accepted source syntax.

func parseMutable(t *testing.T, src string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	return fset, file
}

func receiverIndices(file *ast.File) *ast.IndexListExpr {
	return file.Decls[0].(*ast.FuncDecl).Recv.List[0].Type.(*ast.IndexListExpr)
}

func TestMutatedBadExprReceiverArgument(t *testing.T) {
	const src = "package p\n\nfunc (s S[T_x, U_y]) M() {}\n"
	fset, file := parseMutable(t, src)
	list := receiverIndices(file)
	orig := list.Indices[0]
	list.Indices[0] = &ast.BadExpr{From: orig.Pos(), To: orig.End()}
	_, l, err := ledgerFile("P3", "fx", fset, file, []byte(src), nil)
	if err != nil {
		t.Fatal(err)
	}
	at := strings.Index(src, "T_x")
	var sawArg, sawU bool
	for _, r := range l.Records {
		switch {
		case r.Anchor == [2]int{at, at + 3}:
			sawArg = r.Status == "excluded_receiver_argument" && r.Cand == nil && r.Name == ""
		case r.Name == "U_y":
			sawU = r.Status == "candidate" && r.Subtype == "receiver_type_parameter"
		case r.Name == "_":
			t.Errorf("a BadExpr argument fabricated a blank record: %+v", r)
		}
	}
	if !sawArg || !sawU {
		t.Errorf("records %+v: want the BadExpr as a receiver argument and U_y a candidate", l.Records)
	}
}

func TestMutatedNilReceiverIndexIsIntegrityError(t *testing.T) {
	const src = "package p\n\nfunc (s S[T_x, U_y]) M() {}\n"
	fset, file := parseMutable(t, src)
	receiverIndices(file).Indices[1] = nil
	if s, l, err := ledgerFile("P3", "fx", fset, file, []byte(src), nil); err == nil || s != nil || l != nil {
		t.Errorf("nil receiver index: err %v", err)
	}
}

func TestMutatedUndecodableLiteralStreams(t *testing.T) {
	const src = "package p\n\nimport \"errors\"\n\nvar _ = errors.New(\"xx\")\n"
	fset, file := parseMutable(t, src)
	lit := file.Decls[1].(*ast.GenDecl).Specs[0].(*ast.ValueSpec).Values[0].(*ast.CallExpr).Args[0].(*ast.BasicLit)
	lit.Value = `"\q"` // same length; strconv.Unquote rejects it
	streams, l, err := ledgerFile("P2", "fx", fset, file, []byte(src), nil)
	if err != nil {
		t.Fatal(err)
	}
	r := l.Records[0]
	if r.Status != "undecodable" || strings.Join(r.Reasons, ",") != "undecodable,unknown_context" || r.Cand == nil ||
		len(streams) != 1 {
		t.Errorf("record %+v, %d streams: want a streamed undecodable record", r, len(streams))
	}
}
