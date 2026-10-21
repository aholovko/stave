package structural_test

// These fixtures exercise syntax-only admission, including code a type checker
// would reject. Parse errors must still be rejected by both the parser and BuildLedger.

type ledgerFixture struct {
	name, task, src string
	typeInvalid     bool
}

const p2Imports = "package p\n\nimport (\n\t\"errors\"\n\t\"fmt\"\n)\n\n"

var acceptedFixtures = []ledgerFixture{
	// --- P2 literals, nested and multi-argument calls ---
	{name: "p2_literals", task: "P2", src: p2Imports + "var (\n" +
		"\t_ = errors.New(\"Interpreted.\")\n" +
		"\t_ = errors.New(`Raw:`)\n" +
		"\t_ = errors.New(`CR\r\nLF`)\n" +
		"\t_ = errors.New(\"\\u0043annot\")\n" +
		"\t_ = errors.New(\"final\\n\")\n" +
		"\t_ = errors.New(\"Über!\")\n" +
		"\t_ = errors.New(\"\\xff bad utf8\")\n" +
		"\t_ = errors.New(\"\")\n" +
		"\t_ = errors.New(\"HTTP failed\")\n" +
		"\t_ = errors.New(\"retry?\")\n" +
		"\t_ = fmt.Errorf(\"Outer: %w\", errors.New(\"Inner.\"))\n" +
		"\t_ = errors.New(\"a\", \"b\")\n" +
		")\n", typeInvalid: true},
	// --- P2 callee forms ---
	{name: "p2_callees", task: "P2", src: p2Imports + "func f(err error) {\n" +
		"\t_ = (errors.New)(\"Bad.\")\n" +
		"\t_ = ((fmt.Errorf))(\"Bad: %v\", err)\n" +
		"\t_ = ( /*c*/ errors . New )(\"Bad.\")\n" +
		"\t_ = (errors.New)()\n" +
		"\t_ = (errors.New)((\"Bad.\"))\n" +
		"\t_ = (errors).New(\"Bad.\")\n" +
		"\t_ = ((fmt)).Errorf(\"x\")\n" +
		"\t_ = ((errors).New)(\"x\")\n" +
		"\t_ = Errors.New(\"x\")\n" +
		"\t_ = errors.new(\"x\")\n" +
		"\t_ = errors.Errorf(\"x\")\n" +
		"\t_ = fmt.New(\"x\")\n" +
		"\t_ = e.New(\"x\")\n" +
		"\t_ = x.errors.New(\"x\")\n" +
		"\t_ = errors.New((errors.New)())\n" +
		"}\n", typeInvalid: true},
	// --- P2 arguments ---
	{name: "p2_arguments", task: "P2", src: p2Imports + "func f(msg string) {\n" +
		"\t_ = errors.New(msg)\n" +
		"\t_ = errors.New(\"a\" + \"b\")\n" +
		"\t_ = errors.New((\"Bad.\"))\n" +
		"\t_ = errors.New()\n" +
		"}\n", typeInvalid: true},
	// --- P2 import forms ---
	{name: "p2_import_alias", task: "P2", src: "package p\n\nimport e \"errors\"\n\nvar _ = errors.New(\"x\")\nvar _ = e.New(\"x\")\n", typeInvalid: true},
	{name: "p2_import_same_alias", task: "P2", src: "package p\n\nimport errors \"errors\"\n\nvar _ = errors.New(\"x\")\n"},
	{name: "p2_import_blank", task: "P2", src: "package p\n\nimport _ \"errors\"\n\nvar _ = errors.New(\"x\")\n", typeInvalid: true},
	{name: "p2_import_dot", task: "P2", src: "package p\n\nimport . \"errors\"\n\nvar _ = errors.New(\"x\")\n", typeInvalid: true},
	{name: "p2_import_order_plain_first", task: "P2", src: "package p\n\nimport \"errors\"\nimport e \"errors\"\n\nvar _ = errors.New(\"x\")\nvar _ = e.New(\"x\")\n"},
	{name: "p2_import_order_alias_first", task: "P2", src: "package p\n\nimport e \"errors\"\nimport \"errors\"\n\nvar _ = errors.New(\"x\")\nvar _ = e.New(\"x\")\n"},
	{name: "p2_import_raw_path", task: "P2", src: "package p\n\nimport `errors`\n\nvar _ = errors.New(\"x\")\n"},
	{name: "p2_import_escaped_path", task: "P2", src: "package p\n\nimport \"\\x65rrors\"\n\nvar _ = errors.New(\"x\")\n"},
	{name: "p2_no_import", task: "P2", src: "package p\n\nvar _ = errors.New(\"x\")\nvar _ = fmt.Errorf(\"x\")\n", typeInvalid: true},
	// --- P2 file conflicts, each per base ---
	{name: "p2_conflict_variable", task: "P2", src: p2Imports + "func f() {\n\terrors := 1\n\t_ = errors\n}\n\nvar _ = errors.New(\"x\")\nvar _ = fmt.Errorf(\"x\")\n"},
	{name: "p2_conflict_parameter", task: "P2", src: p2Imports + "func f(fmt int) {}\n\nvar _ = errors.New(\"x\")\nvar _ = fmt.Errorf(\"x\")\n"},
	{name: "p2_conflict_field", task: "P2", src: p2Imports + "type T struct{ errors int }\n\nvar _ = errors.New(\"x\")\nvar _ = fmt.Errorf(\"x\")\n"},
	{name: "p2_conflict_label", task: "P2", src: p2Imports + "func f() {\nerrors:\n\tfor {\n\t\tbreak errors\n\t}\n\t_ = errors.New(\"x\")\n\t_ = fmt.Errorf(\"x\")\n}\n"},
	{name: "p2_conflict_type_param", task: "P2", src: p2Imports + "func g[errors any]() {}\n\nvar _ = errors.New(\"x\")\nvar _ = fmt.Errorf(\"x\")\n"},
	{name: "p2_conflict_receiver_type_param", task: "P2", src: p2Imports + "type S[T any] struct{}\n\nfunc (s S[errors]) M() {\n\t_ = errors.New(\"x\")\n\t_ = fmt.Errorf(\"x\")\n}\n", typeInvalid: true},
	{name: "p2_no_conflict_star_receiver_arg", task: "P2", src: p2Imports + "type S[T any] struct{}\n\nfunc (s S[*errors]) M() {\n\t_ = errors.New(\"x\")\n}\n", typeInvalid: true},
	// --- P2 import-name occurrences ---
	{name: "p2_alias_other_path", task: "P2", src: "package p\n\nimport \"errors\"\nimport errors \"example.com/e\"\n\nvar _ = errors.New(\"x\")\n", typeInvalid: true},
	{name: "p2_alias_other_path_local", task: "P2", src: "package p\n\nimport \"errors\"\nimport errors \"example.com/e\"\n\nfunc f() {\n\terrors := 1\n\t_ = errors\n}\n\nvar _ = errors.New(\"x\")\n", typeInvalid: true},
	{name: "p2_alias_only_local", task: "P2", src: "package p\n\nimport errors \"example.com/e\"\n\nfunc f() {\n\terrors := 1\n\t_ = errors\n}\n\nvar _ = errors.New(\"x\")\n", typeInvalid: true},
	// --- P3 branches and Field list kinds ---
	{name: "p3_branches", task: "P3", src: "package p\n\nimport (\n\tu_a \"fmt\"\n\t_ \"embed\"\n\t. \"strings\"\n)\n\n" +
		"const (\n\tC_a = iota\n\tC_b\n)\n\n" +
		"var V_a, V_b int = 1, 2\nvar V_c = 3\n\n" +
		"// T_a doc.\ntype T_a struct {\n\tF_a, F_b int `tag`\n\tEmbedded\n}\n\n" +
		"type I_a interface {\n\tM_a(p_a int) (r_a int)\n\tEmbedded\n}\n\n" +
		"type G_a[P_a any, Q_b comparable] = map[P_a]Q_b\n\n" +
		"// F_a doc.\nfunc F_a[TP_a any](p_b int, _ string) (r_b, r_c int) {\n" +
		"\tvar local_a int = 1\n" +
		"L_a:\n\tfor i_a := 0; ; {\n\t\tbreak L_a\n\t}\n" +
		"\tif x_a := 1; x_a > 0 {\n\t}\n" +
		"\tfor k_a, v_a := range []int{} {\n\t}\n" +
		"\tfor local_a = range []int{} {\n\t}\n" +
		"\tswitch s_a := 1; s_a {\n\t}\n" +
		"\tswitch t_a := any(1).(type) {\n\t}\n" +
		"\tselect {\n\tcase r_d := <-ch:\n\t\t_ = r_d\n\t}\n" +
		"\tx_a, y_a := 1, 2\n" +
		"\tlocal_a, z_a := 3, 4\n" +
		"\tf_a := func(lp_a int) (lr_a int) { return }\n" +
		"\tuse(u_a, x_a, y_a, z_a, f_a)\n" +
		"\treturn\n}\n\n" +
		"func (r_e *T_a) M_b() {}\n\nfunc Bodiless_a(p_c int)\n\nfunc TestFoo_Bar() {}\n\n" +
		"var ÿ_x, a__b, _a, a_ = 1, 2, 3, 4\n\nvar A_x, B_y int\n", typeInvalid: true},
	{name: "p3_crlf", task: "P3", src: "package p\r\n\r\nfunc F_a(\r\n\tp_a int,\r\n) {\r\n\tv_a := `x\r\ny`\r\n\t_ = v_a\r\n}\r\n"},
	{name: "p3_line_directive", task: "P3", src: "package p\n\n//line other.go:100:1\nfunc L_a(q_a int) {}\n"},
	// --- P3 receiver supplement ---
	{name: "p3_receivers", task: "P3", src: "package p\n\ntype S[T any] struct{}\n\n" +
		"func (s S[T_x]) A() {}\n" +
		"func (s *S[T_x]) B() {}\n" +
		"func (S[T_x]) C() {}\n" +
		"func (*S[T_x]) D() {}\n" +
		"func (s p.S[T_x]) E() {}\n" +
		"func (s S[int]) F() {}\n" +
		"func (s S[K_x, V]) G() {}\n" +
		"func (s (*( /*c*/\r\n\tS[T_x]))) H() {}\n" +
		"func (s S[_, _T, T_, A__B]) I() {}\n", typeInvalid: true},
	{name: "p3_receiver_exclusions", task: "P3", src: "package p\n\ntype S[T any] struct{}\n\n" +
		"func (s S[*T_x]) A() {}\n" +
		"func (s S[[]int]) B() {}\n" +
		"func (s S[p.T_x]) C() {}\n" +
		"func (s S[struct{ f_x int }]) D() {}\n" +
		"func (s S[func(p_q int)]) E() {}\n" +
		"func (S[U_y, F(x)]) F() {}\n", typeInvalid: true},
	{name: "p3_receiver_nothing", task: "P3", src: "package p\n\ntype S[T any] struct{}\n\n" +
		"func (s **S[T_x]) A() {}\n" +
		"func (s ...S[T_x]) B() {}\n" +
		"func (s []S[T_x]) C() {}\n" +
		"func () D() {}\n" +
		"func (a S[A_x], b S[B_x]) E() {}\n" +
		"func H(p S[I_x]) S[J_x] { return S[M_x]{} }\n" +
		"type R struct{ f S[K_y] }\n" +
		"var v S[L_x]\n" +
		"type N interface{ S[N_x] }\n" +
		"func G[T_x any]() {}\n" +
		"type Q[T_x any] struct{}\n", typeInvalid: true},
}

var parseErrorFixtures = []ledgerFixture{
	{name: "p2_invalid_escape", task: "P2", src: p2Imports + "var _ = errors.New(\"\\q\")\n"},
	{name: "p2_unterminated", task: "P2", src: p2Imports + "var _ = errors.New(\"x)\n"},
	{name: "p3_empty_receiver_index", task: "P3", src: "package p\n\nfunc (s S[]) M() {}\n"},
	{name: "p3_invalid_utf8", task: "P3", src: "package p\n\nvar a\xff_b int\n"},
}

// handSpan is a hand-written expectation: the exact envelope text, which must occur once in the fixture, and the CAND
// text inside it. When the envelope text alone is not unique, bars mark it inside unique surrounding context.
type handSpan struct {
	fixture, env, cand, status, subtype string
}

var handSpans = []handSpan{
	{"p2_callees", "|(errors.New)(\"Bad.\"|)\n", "\"Bad.\"", "unknown_context", ""},
	{"p2_callees", "((fmt.Errorf))(\"Bad: %v\"", "\"Bad: %v\"", "unknown_context", ""},
	{"p2_callees", "( /*c*/ errors . New )(\"Bad.\"", "\"Bad.\"", "unknown_context", ""},
	{"p2_literals", "errors.New(`CR\r\nLF`", "`CR\r\nLF`", "unknown_context", ""},
	{"p2_literals", "fmt.Errorf(\"Outer: %w\"", "\"Outer: %w\"", "unknown_context", ""},
	{"p2_literals", "errors.New(\"a\"", "\"a\"", "unknown_context", ""},
	{"p3_receivers", "s (*( /*c*/\r\n\tS[T_x]))", "T_x", "candidate", "receiver_type_parameter"},
	{"p3_receivers", "func (|S[T_x]|) C", "T_x", "candidate", "receiver_type_parameter"},
	{"p3_receivers", "s p.S[T_x]", "T_x", "candidate", "receiver_type_parameter"},
	{"p3_receivers", "s S[int]", "int", "candidate", "receiver_type_parameter"},
	{"p3_receiver_exclusions", "S[U_y, F(x)]", "U_y", "candidate", "receiver_type_parameter"},
	{"p3_receiver_exclusions", "f_x int", "f_x", "candidate", "struct_field"},
	{"p3_receiver_exclusions", "p_q int", "p_q", "candidate", "param"},
	{"p3_branches", "u_a \"fmt\"", "u_a", "candidate", "import"},
	{"p3_branches", "C_a", "C_a", "candidate", "const"},
	{"p3_branches", "V_a, V_b int", "V_b", "candidate", "var"},
	{"p3_branches", "V_c", "V_c", "candidate", "var"},
	{"p3_branches", "T_a struct {\n\tF_a, F_b int `tag`\n\tEmbedded\n}", "T_a", "candidate", "type"},
	{"p3_branches", "F_a, F_b int `tag`", "F_b", "candidate", "struct_field"},
	{"p3_branches", "M_a(p_a int) (r_a int)", "M_a", "candidate", "interface_method"},
	{"p3_branches", "(|r_a int|)", "r_a", "candidate", "result"},
	{"p3_branches", "G_a[P_a any, Q_b comparable] = map[P_a]Q_b", "G_a", "candidate", "type"},
	{"p3_branches", "[|P_a any|,", "P_a", "candidate", "type_param"},
	{"p3_branches", "func F_a[TP_a any](p_b int, _ string) (r_b, r_c int)", "F_a", "candidate", "func"},
	{"p3_branches", "local_a int", "local_a", "candidate", "var"},
	{"p3_branches", "L_a:", "L_a", "candidate", "label"},
	{"p3_branches", "i_a :=", "i_a", "candidate", "define"},
	{"p3_branches", "k_a, v_a :=", "v_a", "candidate", "range"},
	{"p3_branches", "t_a :=", "t_a", "candidate", "define"},
	{"p3_branches", "r_d :=", "r_d", "candidate", "define"},
	{"p3_branches", "local_a, z_a :=", "local_a", "candidate", "define"},
	{"p3_branches", "r_e *T_a", "r_e", "candidate", "receiver"},
	{"p3_branches", "func (r_e *T_a) M_b()", "M_b", "candidate", "method"},
	{"p3_branches", "func Bodiless_a(p_c int)", "Bodiless_a", "candidate", "func"},
	{"p3_branches", "func TestFoo_Bar()", "TestFoo_Bar", "candidate", "func"},
	{"p3_branches", "ÿ_x, a__b, _a, a_", "a_", "candidate", "var"},
	{"p3_branches", "A_x, B_y int", "A_x", "candidate", "var"},
	{"p3_branches", "A_x, B_y int", "B_y", "candidate", "var"},
	{"p3_crlf", "p_a int", "p_a", "candidate", "param"},
	{"p3_crlf", "v_a :=", "v_a", "candidate", "define"},
	{"p3_line_directive", "func L_a(q_a int)", "L_a", "candidate", "func"},
}
