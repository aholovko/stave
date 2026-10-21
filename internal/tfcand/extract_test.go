package tfcand

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

// rows extracts src and indexes the call rows by their target text.
func rows(t *testing.T, src string) (FileRow, map[string]CallRow) {
	t.Helper()
	p := Screen("/x_test.go", []byte(src))
	byText := map[string]CallRow{}
	for _, c := range p.Rows {
		text := src[c.Target.Start:c.Target.End]
		if _, dup := byText[text]; dup {
			t.Fatalf("duplicate target text %q", text)
		}
		byText[text] = c
	}
	return p.Row, byText
}

func row(t *testing.T, byText map[string]CallRow, text string) CallRow {
	t.Helper()
	c, ok := byText[text]
	if !ok {
		keys := make([]string, 0, len(byText))
		for k := range byText {
			keys = append(keys, k)
		}
		t.Fatalf("no row for %q; have %q", text, keys)
	}
	return c
}

func admitted(t *testing.T, c CallRow) {
	t.Helper()
	if !c.Admitted || c.Reason != nil {
		t.Fatalf("want admitted, got reason %v", deref(c.Reason))
	}
}

func excluded(t *testing.T, c CallRow, reason string) {
	t.Helper()
	if c.Admitted || c.Reason == nil || *c.Reason != reason {
		t.Fatalf("want excluded %q, got admitted=%v reason=%v", reason, c.Admitted, deref(c.Reason))
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func sliceOf(src string, s Slice) string { return src[s.Start:s.End] }

func TestDirectTAndBCallsAreAdmittedWithSpans(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\n// TestA checks.\nfunc TestA(t *testing.T) {\n\tt.Errorf(\"got %d\",\n\t\t1)\n\tt.Fatal(\"x\")\n}\n\nfunc BenchmarkB(b *testing.B) {\n\tb.Error(\"y\")\n\tb.Fatalf(\"z\")\n}\n"
	file, byText := rows(t, src)
	if file.Status != "checked" || file.Admitted != 4 || file.Calls != 4 {
		t.Fatalf("file row %+v", file)
	}
	c := row(t, byText, "t.Errorf(\"got %d\",\n\t\t1)")
	admitted(t, c)
	if c.Method != "Errorf" || c.Binding.Kind != "T" || c.Binding.Name != "t" || c.Statement != "expression" {
		t.Fatalf("row %+v", c)
	}
	if c.Target.Line != 7 || c.Target.SHA256 != digest([]byte(sliceOf(src, c.Target))) {
		t.Fatalf("target %+v", c.Target)
	}
	decl := sliceOf(src, c.Declaration.Span)
	if !strings.HasPrefix(decl, "// TestA checks.\nfunc TestA") || !strings.HasSuffix(decl, "}") {
		t.Fatalf("declaration %q", decl)
	}
	if c.Declaration.Span.Line != 5 || c.Declaration.Name != "TestA" || c.Declaration.Kind != "func" {
		t.Fatalf("declaration %+v", c.Declaration)
	}
	if sliceOf(src, c.Binding.Field) != "t *testing.T" || c.Binding.Owner.Kind != "declaration" {
		t.Fatalf("binding %+v", c.Binding)
	}
	if *c.CallerContext != "complete" || *c.Callback || len(c.ContextReasons) != 0 || c.EnclosingRunName.Kind != "none" {
		t.Fatalf("context %+v", c)
	}
	b := row(t, byText, "b.Fatalf(\"z\")")
	admitted(t, b)
	if b.Binding.Kind != "B" || b.Method != "Fatalf" || *b.CallerContext != "complete" {
		t.Fatalf("row %+v", b)
	}
}

func TestAliasedImportIsRecognizedByPath(t *testing.T) {
	src := "package p\n\nimport tt \"testing\"\n\nfunc TestA(t *tt.T) {\n\tt.Error(\"x\")\n}\n"
	file, byText := rows(t, src)
	admitted(t, row(t, byText, "t.Error(\"x\")"))
	if !reflect.DeepEqual(file.TestingImports, []string{"tt"}) {
		t.Fatalf("imports %v", file.TestingImports)
	}
}

func TestNotTestingPackageNamedTesting(t *testing.T) {
	src := "package p\n\nimport (\n\t\"testing\"\n\ttesting2 \"example.com/testing\"\n)\n\nvar _ testing.TB\n\nfunc TestA(t *testing2.T) {\n\tt.Error(\"x\")\n}\n"
	_, byText := rows(t, src)
	excluded(t, row(t, byText, "t.Error(\"x\")"), "non_testing_type")
}

func TestHelperIsOutsidePacketWithUnavailableName(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\nfunc check(t *testing.T, v int) {\n\tt.Helper()\n\tt.Errorf(\"v=%d\", v)\n}\n\nfunc Testing(t *testing.T) {\n\tt.Error(\"lower\")\n}\n"
	_, byText := rows(t, src)
	c := row(t, byText, "t.Errorf(\"v=%d\", v)")
	admitted(t, c)
	if *c.CallerContext != "outside_packet" || !reflect.DeepEqual(c.ContextReasons, []string{"helper"}) {
		t.Fatalf("context %v %v", *c.CallerContext, c.ContextReasons)
	}
	if c.EnclosingRunName.Kind != "unavailable" {
		t.Fatalf("run name %+v", c.EnclosingRunName)
	}
	excluded(t, row(t, byText, "t.Helper()"), "ignored_method")
	lower := row(t, byText, "t.Error(\"lower\")")
	if !reflect.DeepEqual(lower.ContextReasons, []string{"helper"}) {
		t.Fatalf("Testing followed by lowercase is not a test: %v", lower.ContextReasons)
	}
}

func TestMethodDeclarationIsOutsidePacket(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\ntype S struct{}\n\nfunc (s *S) TestA(t *testing.T) {\n\tt.Error(\"x\")\n}\n"
	_, byText := rows(t, src)
	c := row(t, byText, "t.Error(\"x\")")
	admitted(t, c)
	if c.Declaration.Kind != "method" || !reflect.DeepEqual(c.ContextReasons, []string{"method"}) {
		t.Fatalf("row %+v", c)
	}
}

func TestNestedSubtestWithOwnParameterAndLiteralName(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {\n\tt.Run(\"sub \\u00e9\", func(t *testing.T) {\n\t\tt.Error(\"inner\")\n\t})\n}\n"
	_, byText := rows(t, src)
	c := row(t, byText, "t.Error(\"inner\")")
	admitted(t, c)
	if c.Binding.Owner.Kind != "literal" || !strings.HasPrefix(sliceOf(src, c.Binding.Owner.Span), "func(t *testing.T)") {
		t.Fatalf("binding %+v", c.Binding)
	}
	if *c.CallerContext != "complete" || *c.Callback {
		t.Fatalf("context %v", *c.CallerContext)
	}
	if len(c.Literals) != 1 || c.Literals[0].Position != "run_argument" {
		t.Fatalf("literals %+v", c.Literals)
	}
	n := c.EnclosingRunName
	if n.Kind != "literal" || *n.Value != "sub é" || sliceOf(src, *n.Span) != "\"sub \\u00e9\"" {
		t.Fatalf("run name %+v", n)
	}
}

func TestClosureCaptureIsFlaggedAsCallback(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {\n\tf := func() {\n\t\tt.Error(\"x\")\n\t}\n\tf()\n}\n"
	_, byText := rows(t, src)
	c := row(t, byText, "t.Error(\"x\")")
	admitted(t, c)
	if c.Binding.Owner.Kind != "declaration" {
		t.Fatalf("binding owner %+v", c.Binding.Owner)
	}
	if !*c.Callback || *c.CallerContext != "outside_packet" || !reflect.DeepEqual(c.ContextReasons, []string{"callback"}) {
		t.Fatalf("context %v %v %v", *c.Callback, *c.CallerContext, c.ContextReasons)
	}
	if len(c.Literals) != 1 || c.Literals[0].Position != "other" || c.EnclosingRunName.Kind != "unavailable" {
		t.Fatalf("literals %+v name %+v", c.Literals, c.EnclosingRunName)
	}
}

func TestShadowingFollowsBlockScopeOrder(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\ntype fake struct{}\n\nfunc (fake) Error(...any) {}\n\nfunc TestA(t *testing.T) {\n\tif true {\n\t\tt.Error(\"before\")\n\t\tt := fake{}\n\t\tt.Error(\"after\")\n\t}\n\tt.Error(\"outer\")\n}\n"
	_, byText := rows(t, src)
	admitted(t, row(t, byText, "t.Error(\"before\")"))
	excluded(t, row(t, byText, "t.Error(\"after\")"), "shadowed")
	admitted(t, row(t, byText, "t.Error(\"outer\")"))
}

func TestReturnedCallbackIsAdmittedWithFlag(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\ntype TestCase struct{ Label string }\n\nfunc genSub(c TestCase) func(t *testing.T) {\n\treturn func(t *testing.T) {\n\t\tif c.Label == \"\" {\n\t\t\tt.Fatal(\"no label\")\n\t\t}\n\t\tt.Errorf(\"%s\", c.Label)\n\t}\n}\n\nfunc TestC(t *testing.T) {\n\tfor _, v := range []TestCase{} {\n\t\tt.Run(v.Label, genSub(v))\n\t}\n}\n"
	_, byText := rows(t, src)
	for _, text := range []string{"t.Fatal(\"no label\")", "t.Errorf(\"%s\", c.Label)"} {
		c := row(t, byText, text)
		admitted(t, c)
		if !*c.Callback || *c.CallerContext != "outside_packet" || !reflect.DeepEqual(c.ContextReasons, []string{"callback", "helper"}) {
			t.Fatalf("%s: %v %v", text, *c.CallerContext, c.ContextReasons)
		}
		if c.Literals[0].Position != "returned" || c.EnclosingRunName.Kind != "unavailable" {
			t.Fatalf("%s: %+v %+v", text, c.Literals, c.EnclosingRunName)
		}
		if !strings.HasPrefix(sliceOf(src, c.Declaration.Span), "func genSub") {
			t.Fatalf("declaration %q", sliceOf(src, c.Declaration.Span))
		}
	}
	excluded(t, row(t, byText, "t.Run(v.Label, genSub(v))"), "ignored_method")
}

func TestTableLoopDynamicSubtestName(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\nfunc TestT(t *testing.T) {\n\tcases := []struct{ name string }{{\"a\"}}\n\tfor _, tc := range cases {\n\t\tt.Run(tc.name, func(t *testing.T) {\n\t\t\tt.Errorf(\"case %s\", tc.name)\n\t\t})\n\t}\n}\n"
	_, byText := rows(t, src)
	c := row(t, byText, "t.Errorf(\"case %s\", tc.name)")
	admitted(t, c)
	n := c.EnclosingRunName
	if n.Kind != "dynamic" || n.Value != nil || sliceOf(src, *n.Span) != "tc.name" || *c.CallerContext != "complete" {
		t.Fatalf("run name %+v context %v", n, *c.CallerContext)
	}
}

func TestOtherLiteralPositionsAreCallbacks(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\nfunc FuzzF(f *testing.F) {\n\tf.Fuzz(func(t *testing.T, s string) {\n\t\tt.Error(s)\n\t})\n}\n\nfunc TestR(t *testing.T) {\n\tt.Run(\"n\", func(t *testing.T) {\n\t\tt.Cleanup(func() {\n\t\t\tt.Error(\"c\")\n\t\t})\n\t})\n}\n"
	_, byText := rows(t, src)
	fz := row(t, byText, "t.Error(s)")
	admitted(t, fz)
	if !reflect.DeepEqual(fz.ContextReasons, []string{"callback", "helper"}) || fz.Literals[0].Position != "other" {
		t.Fatalf("fuzz %+v", fz)
	}
	c := row(t, byText, "t.Error(\"c\")")
	admitted(t, c)
	if !reflect.DeepEqual(c.ContextReasons, []string{"callback"}) || c.EnclosingRunName.Kind != "literal" {
		t.Fatalf("cleanup %+v %+v", c.ContextReasons, c.EnclosingRunName)
	}
	if len(c.Literals) != 2 || c.Literals[0].Position != "run_argument" || c.Literals[1].Position != "other" {
		t.Fatalf("literals %+v", c.Literals)
	}
}

func TestReceiverExclusions(t *testing.T) {
	src := "package p\n\nimport (\n\t\"errors\"\n\t\"log\"\n\t\"testing\"\n)\n\ntype suite struct{ t *testing.T }\n\nfunc h(tb testing.TB, s *suite, e error) {\n\ttb.Error(\"tb\")\n\ts.t.Error(\"field\")\n\t_ = e.Error()\n\tlog.Fatal(\"log\")\n}\n\nfunc TestE(t *testing.T) {\n\t(t).Error(\"paren\")\n\tt.Log(\"log\")\n\tt.Skip(\"skip\")\n\tt.Error()\n\tdefer t.Error(\"deferred\")\n\tgo t.Fatal(\"go\")\n\tf := t.Errorf\n\tf(\"alias\")\n\ttt := t\n\ttt.Error(\"alias value\")\n\t_ = errors.New(\"x\")\n}\n"
	file, byText := rows(t, src)
	excluded(t, row(t, byText, "tb.Error(\"tb\")"), "testing_TB")
	excluded(t, row(t, byText, "s.t.Error(\"field\")"), "unsupported_receiver_form")
	excluded(t, row(t, byText, "e.Error()"), "non_testing_type")
	excluded(t, row(t, byText, "log.Fatal(\"log\")"), "unresolved_receiver")
	excluded(t, row(t, byText, "(t).Error(\"paren\")"), "unsupported_receiver_form")
	excluded(t, row(t, byText, "t.Log(\"log\")"), "ignored_method")
	excluded(t, row(t, byText, "t.Skip(\"skip\")"), "ignored_method")
	excluded(t, row(t, byText, "t.Error()"), "empty_arguments")
	excluded(t, row(t, byText, "t.Error(\"deferred\")"), "deferred_or_go")
	excluded(t, row(t, byText, "t.Fatal(\"go\")"), "deferred_or_go")
	excluded(t, row(t, byText, "tt.Error(\"alias value\")"), "unresolved_receiver")
	mv := row(t, byText, "t.Errorf")
	excluded(t, mv, "method_value")
	if mv.Unit != "selector" {
		t.Fatalf("method value unit %q", mv.Unit)
	}
	if _, ok := byText["f(\"alias\")"]; ok {
		t.Fatal("a call through a method value is not examined")
	}
	if file.Admitted != 0 || file.Selectors != 1 || file.Calls != len(byText)-1 {
		t.Fatalf("file counts %+v", file)
	}
	if file.Exclusions["ignored_method"] != 2 || file.Exclusions["method_value"] != 1 {
		t.Fatalf("exclusions %v", file.Exclusions)
	}
}

func TestParenthesizedFailureCallIsExaminedOnce(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\nfunc g(func(...any)) {}\n\nfunc TestP(t *testing.T) {\n\t(t.Error)(\"x\")\n\t((t.Fatalf))(\"y %d\", 1)\n\tdefer (t.Error)(\"d\")\n\tgo ((t.Fatal))(\"g\")\n\tf := (t.Errorf)\n\tf(\"v\")\n\tg((t.Error))\n}\n"
	file, byText := rows(t, src)
	for _, text := range []string{"(t.Error)(\"x\")", "((t.Fatalf))(\"y %d\", 1)"} {
		c := row(t, byText, text)
		admitted(t, c)
		if c.Unit != "call" || c.Statement != "expression" || *c.Receiver != "t" || *c.CallerContext != "complete" {
			t.Fatalf("%s: %+v", text, c)
		}
		if src[c.Sel:c.Sel+len(c.Method)] != c.Method {
			t.Fatalf("%s: selector offset %d", text, c.Sel)
		}
	}
	d := row(t, byText, "(t.Error)(\"d\")")
	excluded(t, d, "deferred_or_go")
	g := row(t, byText, "((t.Fatal))(\"g\")")
	excluded(t, g, "deferred_or_go")
	if d.Statement != "defer" || g.Statement != "go" {
		t.Fatalf("statements %q %q", d.Statement, g.Statement)
	}
	excluded(t, row(t, byText, "t.Errorf"), "method_value")
	if mv := row(t, byText, "t.Error"); mv.Unit != "selector" || *mv.Reason != "method_value" {
		t.Fatalf("argument method value %+v", mv)
	}
	if len(byText) != 6 || file.Calls != 4 || file.Selectors != 2 || file.Admitted != 2 ||
		file.Exclusions["deferred_or_go"] != 2 || file.Exclusions["method_value"] != 2 {
		t.Fatalf("file counts %+v rows %d", file, len(byText))
	}
}

func TestImportFlagsAndParameterBinding(t *testing.T) {
	noImport := "package p\n\ntype T struct{}\n\nfunc (T) Error(...any) {}\n\nfunc h(t *T) {\n\tt.Error(\"x\")\n}\n"
	_, byText := rows(t, noImport)
	excluded(t, row(t, byText, "t.Error(\"x\")"), "no_testing_import")

	dot := "package p\n\nimport . \"testing\"\n\nfunc TestD(t *T) {\n\tt.Error(\"x\")\n}\n"
	file, byText := rows(t, dot)
	excluded(t, row(t, byText, "t.Error(\"x\")"), "dot_import")
	if !file.DotImport {
		t.Fatal("dot import flag")
	}

	blank := "package p\n\nimport _ \"testing\"\n\nfunc h(t *testing.T) {\n\tt.Error(\"x\")\n}\n"
	file, byText = rows(t, blank)
	excluded(t, row(t, byText, "t.Error(\"x\")"), "no_testing_import")
	if !file.BlankImport || len(file.TestingImports) != 0 {
		t.Fatalf("blank import %+v", file)
	}

	grouped := "package p\n\nimport \"testing\"\n\nfunc h(a, b *testing.T) (r *testing.T) {\n\ta.Error(\"a\")\n\tb.Error(\"b\")\n\tr.Error(\"r\")\n\treturn nil\n}\n"
	_, byText = rows(t, grouped)
	admitted(t, row(t, byText, "a.Error(\"a\")"))
	admitted(t, row(t, byText, "b.Error(\"b\")"))
	excluded(t, row(t, byText, "r.Error(\"r\")"), "non_testing_type")
}

func TestPackageLevelLiteralHasNoDeclaration(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\nvar check = func(t *testing.T) {\n\tt.Error(\"x\")\n}\n"
	_, byText := rows(t, src)
	excluded(t, row(t, byText, "t.Error(\"x\")"), "no_enclosing_declaration")
}

func TestCRLFAndUnicodeSpansAreOriginalBytes(t *testing.T) {
	src := "package p\r\n\r\nimport \"testing\"\r\n\r\n// Tést doc\r\nfunc TestU(t *testing.T) {\r\n\tt.Errorf(`é\r\nraw`,\r\n\t\t\"ü\")\r\n}\r\n"
	_, byText := rows(t, src)
	c := row(t, byText, "t.Errorf(`é\r\nraw`,\r\n\t\t\"ü\")")
	admitted(t, c)
	decl := sliceOf(src, c.Declaration.Span)
	if !strings.HasPrefix(decl, "// Tést doc\r\nfunc TestU") || !strings.HasSuffix(decl, "\r\n}") {
		t.Fatalf("declaration %q", decl)
	}
	if c.Target.Line != 7 || c.Declaration.Span.Line != 5 {
		t.Fatalf("lines %d %d", c.Target.Line, c.Declaration.Span.Line)
	}
}

func TestFileStatuses(t *testing.T) {
	gen := "// Code generated by mockgen. DO NOT EDIT.\n\npackage p\n\nimport \"testing\"\n\nfunc TestG(t *testing.T) { t.Error(\"x\") }\n"
	p := Screen("/g_test.go", []byte(gen))
	if p.Row.Status != "generated" || !p.Row.Generated || len(p.Rows) != 0 {
		t.Fatalf("generated %+v %d", p.Row, len(p.Rows))
	}
	bad := "package p\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) { t.Error(\"x\") \n"
	p = Screen("/b_test.go", []byte(bad))
	if p.Row.Status != "parse_error" || p.Row.Reason == nil || len(p.Rows) != 0 {
		t.Fatalf("parse error %+v %d", p.Row, len(p.Rows))
	}
	p = Screen("/u_test.go", []byte("package p\n// \xff\n"))
	if p.Row.Status != "invalid_utf8" || len(p.Rows) != 0 || p.Row.Package != nil {
		t.Fatalf("invalid utf8 %+v", p.Row)
	}
	none := "package p\n\nfunc f() {}\n"
	p = Screen("/n_test.go", []byte(none))
	if p.Row.Status != "checked" || p.Row.Calls != 0 || len(p.Rows) != 0 || p.Row.Exclusions == nil {
		t.Fatalf("no candidates %+v", p.Row)
	}
}

func digestHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestSliceHashesMatchSourceBytes(t *testing.T) {
	src := "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {\n\tt.Run(\"n\", func(t *testing.T) { t.Error(\"x\") })\n}\n"
	_, byText := rows(t, src)
	c := row(t, byText, "t.Error(\"x\")")
	for _, s := range []Slice{c.Target, c.Declaration.Span, c.Binding.Field, c.Binding.Owner.Span, *c.EnclosingRunName.Span, c.Literals[0].Span} {
		if s.SHA256 != digestHex(sliceOf(src, s)) {
			t.Fatalf("slice %+v", s)
		}
	}
}
