package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aholovko/stave/internal/structural"
)

// Goldens retain the research output with class IDs renamed to P1/P2/P3 and the source ID e1 renamed to p1.
// The certificate's absolute package directory is normalized to $FIXTURE_DIR.
var structuralGolden = []struct {
	golden, task, input string
	args                []string
}{
	{"p1.P1", "P1", "internal/structural/testdata/p1.txt", nil},
	{"p1.P2", "P2", "internal/structural/testdata/p1.txt", nil},
	{"p1.P3", "P3", "internal/structural/testdata/p1.txt", nil},
	{"p1.DC", "DC", "internal/structural/testdata/p1.txt", nil},
	{"p1.RP", "RP", "internal/structural/testdata/p1.txt", nil},
	{"demo.P1", "P1", "testdata/fixture-pkg/demo.go", nil},
	{"demo.P2", "P2", "testdata/fixture-pkg/demo.go", nil},
	{"demo.P2target", "P2", "testdata/fixture-pkg/demo.go", []string{"--target", "testdata/fixture-pkg/demo.go"}},
	{"demo.P3", "P3", "testdata/fixture-pkg/demo.go", nil},
	{"demo.DC", "DC", "testdata/fixture-pkg/demo.go", nil},
	{"demo.RP", "RP", "testdata/fixture-pkg/demo.go", nil},
	{"dc.DC", "DC", "internal/structural/testdata/dc.txt", nil},
	{"rp.RP", "RP", "internal/structural/testdata/rp.txt", nil},
	{"demo_test.TF", "TF", "testdata/fixture-pkg/demo_test.go", []string{"--path", "fixture-pkg/demo_test.go"}},
	{"external_test.TF", "TF", "testdata/fixture-pkg/external_test.go", []string{"--path", "fixture-pkg/external_test.go"}},
	{"tf_direct.TF", "TF", "internal/structural/testdata/tf_direct.txt", []string{"--path", "p/x_test.go"}},
	{"tf_binding.TF", "TF", "internal/structural/testdata/tf_binding.txt", []string{"--path", "p/x_test.go"}},
	{"tf_receivers.TF", "TF", "internal/structural/testdata/tf_receivers.txt", []string{"--path", "p/x_test.go"}},
	{"tf_scopes.TF", "TF", "internal/structural/testdata/tf_scopes.txt", []string{"--path", "p/x_test.go"}},
}

func TestStructuralMatchesRetainedGolden(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	fixtureDir, err := filepath.Abs(filepath.Join("testdata", "fixture-pkg"))
	if err != nil {
		t.Fatal(err)
	}
	quoted, err := json.Marshal(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	escapedDir := quoted[1 : len(quoted)-1]

	for _, c := range structuralGolden {
		t.Run(c.golden, func(t *testing.T) {
			src, err := os.ReadFile(c.input)
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"--task", c.task, "--id", strings.Split(c.golden, ".")[0]}
			ledger := ""
			if c.task == "P2" || c.task == "P3" || c.task == "TF" {
				ledger = filepath.Join(t.TempDir(), "ledger.json")
				args = append(args, "--ledger", ledger)
			}
			if c.task == "TF" {
				args = append(args, "--sha256", shaOf(string(src)))
			}
			args = append(args, c.args...)

			var stdout, stderr bytes.Buffer
			if code := runStructural(args, bytes.NewReader(src), &stdout, &stderr); code != 0 {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			base := filepath.Join("internal", "structural", "testdata", "golden", c.golden)
			sameAsGolden(t, base+".stdout", stdout.Bytes())
			sameAsGolden(t, base+".stderr", stderr.Bytes())
			if ledger != "" {
				got, err := os.ReadFile(ledger)
				if err != nil {
					t.Fatal(err)
				}
				sameAsGolden(t, base+".ledger", bytes.ReplaceAll(got, escapedDir, []byte("$FIXTURE_DIR")))
			}
		})
	}
}

func sameAsGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden output\n got %q\nwant %q", filepath.Base(path), got, want)
	}
}

const selectorSource = "package p\n\n// F doc.\nfunc F(a int) int {\n\tif a > 0 {\n\t\treturn 1\n\t} else {\n\t\ta--\n\t}\n\treturn a\n}\n"

const tfSource = "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T, e error) {\n\tt.Error(\"a\")\n\tt.Run(\"n\", func(u *testing.T) { u.Fatal(\"b\") })\n\tg := e.Error\n\t_ = g\n}\n"

// runTF runs the TF mode on src and returns the exit code, stdout and the ledger document (nil when no ledger was
// written).
func runTF(t *testing.T, src, path, sha string) (int, string, map[string]any) {
	t.Helper()
	ledger := filepath.Join(t.TempDir(), "ledger.json")
	var stdout, stderr bytes.Buffer
	code := runStructural([]string{"--task", "TF", "--id", "fx", "--ledger", ledger, "--path", path, "--sha256", sha},
		strings.NewReader(src), &stdout, &stderr)
	raw, err := os.ReadFile(ledger)
	if err != nil {
		return code, stdout.String(), nil
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return code, stdout.String(), doc
}

func TestStructuralIsAStaveSubcommand(t *testing.T) {
	code, stdout, stderr := stave(t, "structural", "--task", "X")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "--task P1|DC|RP") {
		t.Errorf("exit %d, stdout %q, stderr %q; want the structural usage", code, stdout, stderr)
	}
}

func TestDCRPWriteOrdinaryResultsAndWithheldCounts(t *testing.T) {
	src := "package p\n\n// T doc.\ntype T struct{}\n\n// Al doc.\ntype Al = struct{}\n\n// Two doc.\nfunc Two() (int, int) { return 0, 0 }\n"
	for task, want := range map[string]string{"DC": "// T doc.", "RP": "(int, int)"} {
		var stdout, stderr bytes.Buffer
		if code := runStructural([]string{"--task", task, "--id", "fx"}, strings.NewReader(src), &stdout, &stderr); code != 0 {
			t.Fatalf("%s exit %d: %s", task, code, stderr.String())
		}
		var r struct {
			OK         bool
			Task, Cand string
			Roles      map[string]structural.Role
		}
		if err := json.Unmarshal(stdout.Bytes(), &r); err != nil || !r.OK || r.Task != task {
			t.Fatalf("%s result %s: %v", task, stdout.String(), err)
		}
		cand := r.Roles["CAND"].IV[0]
		if src[cand[0]:cand[1]] != want || !strings.HasPrefix(r.Cand, task+":fx:") {
			t.Errorf("%s cand %s %q", task, r.Cand, src[cand[0]:cand[1]])
		}
		if task == "DC" && !strings.Contains(stderr.String(), "DC withheld map[alias:1]") {
			t.Errorf("DC stderr %q", stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := runStructural([]string{"--task", "DC", "--ledger", "x"}, strings.NewReader(src), &stdout, &stderr); code != 2 {
		t.Errorf("DC with --ledger exit %d, want 2", code)
	}
}

func TestTFWritesLedgerAndOneResultPerAdmittedRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "x_test.go")
	code, out, doc := runTF(t, tfSource, path, shaOf(tfSource))
	if code != 0 || doc == nil {
		t.Fatalf("exit %d, ledger %v", code, doc)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var cands []string
	for _, line := range lines {
		var r struct {
			OK         bool
			Task, Cand string
			Roles      map[string]structural.Role
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil || !r.OK || r.Task != "TF" || len(r.Roles) != 8 {
			t.Fatalf("result %s: %v", line, err)
		}
		cands = append(cands, r.Cand)
	}
	if strings.Join(cands, ",") != "TF:fx:0,TF:fx:1" || strings.Contains(out, "x_test.go") || strings.Contains(out, "context") {
		t.Fatalf("results %s", out)
	}
	if doc["schema"] != "stave-tf-ledger-1" || doc["path"] != path || doc["file"].(map[string]any)["admitted"] != 2.0 {
		t.Fatalf("ledger %v", doc)
	}
	sentry := doc["sentry"].(map[string]any)
	if sentry["total"] != 3.0 || sentry["matched"] != 2.0 || sentry["unresolved"] != 1.0 {
		t.Fatalf("sentry %v", sentry)
	}
	again, out2, _ := runTF(t, tfSource, path, shaOf(tfSource))
	if again != 0 || out2 != out {
		t.Fatal("TF output is not deterministic")
	}
}

func TestTFUsageIdentityGuardsAndFileFailures(t *testing.T) {
	sha := shaOf(tfSource)
	ledger := filepath.Join(t.TempDir(), "l.json")
	for _, args := range [][]string{
		{"--task", "TF", "--path", "x_test.go", "--sha256", sha},
		{"--task", "TF", "--ledger", ledger, "--sha256", sha},
		{"--task", "TF", "--ledger", ledger, "--path", "x_test.go"},
		{"--task", "TF", "--ledger", ledger, "--path", "x_test.go", "--sha256", sha, "--target", "x_test.go"},
		{"--task", "P1", "--path", "x_test.go"},
		{"--task", "DC", "--sha256", sha},
		{"--task", "P3", "--ledger", ledger, "--path", "x_test.go"},
	} {
		var stdout, stderr bytes.Buffer
		if code := runStructural(args, strings.NewReader(tfSource), &stdout, &stderr); code != 2 || stdout.Len() != 0 {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
	if code, out, doc := runTF(t, tfSource, "x_test.go", shaOf(tfSource+"x")); code != 1 || out != "" || doc != nil {
		t.Fatalf("identity mismatch: exit %d, output %q, ledger %v", code, out, doc)
	}
	code, out, doc := runTF(t, tfSource, "vendor/x_test.go", sha)
	if code != 0 || out != "" || doc["path_gate"].(map[string]any)["reason"] != "vendor" || doc["file"] != nil {
		t.Fatalf("guarded path: exit %d, output %q, ledger %v", code, out, doc)
	}
	bad := "package p\n\nfunc TestA(t *testing.T) {\n\tt.Error(\"a\")\n"
	code, out, doc = runTF(t, bad, "x_test.go", shaOf(bad))
	hits := doc["sentry"].(map[string]any)["hits"].([]any)
	if code != 0 || out != "" || doc["file"].(map[string]any)["status"] != "parse_error" ||
		hits[0].(map[string]any)["status"] != "unknown" || hits[0].(map[string]any)["form"] != "direct_call" {
		t.Fatalf("parse error: exit %d, output %q, ledger %v", code, out, doc)
	}
}

func TestOneResultLinePerP1Candidate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runStructural([]string{"--task", "P1", "--id", "fx"}, strings.NewReader(selectorSource), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("%d result lines", len(lines))
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &result); err != nil {
		t.Fatal(err)
	}
	if result["ok"] != true || result["cand"] != "P1:fx:0" {
		t.Errorf("result %v", result)
	}
}

func TestStructuralUsageAndParseFailures(t *testing.T) {
	for _, task := range []string{"X", "E9", "DS", "A1", "A2", "A3"} {
		var stdout, stderr bytes.Buffer
		if code := runStructural([]string{"--task", task}, strings.NewReader(selectorSource), &stdout, &stderr); code != 2 ||
			!strings.Contains(stderr.String(), "P1|DC|RP") || stdout.Len() != 0 {
			t.Errorf("task %s: exit %d, usage %q", task, code, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := runStructural([]string{"--task", "P1"}, strings.NewReader("package p\nfunc {"), &stdout, &stderr); code != 1 {
		t.Errorf("unparseable source exit %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("failures wrote results: %q", stdout.String())
	}
}

const p2Source = "package p\n\nimport \"errors\"\n\nvar _ = errors.New(\"Bad.\")\nvar _ = (errors).New(\"x\")\n"

// Keep Facts raw because Fact supports marshaling only.
type resultRow struct {
	OK         bool                       `json:"ok"`
	Task       string                     `json:"task"`
	Cand       string                     `json:"cand"`
	Source     structural.Source          `json:"source"`
	Runs       [][2]int                   `json:"runs"`
	Roles      map[string]structural.Role `json:"roles"`
	Facts      []json.RawMessage          `json:"facts"`
	Reason     string                     `json:"reason"`
	EventIndex *int                       `json:"event_index"`
}

// readLedger decodes the ledger file and the stdout results strictly.
func readLedger(t *testing.T, path string, stdout []byte) (structural.Ledger, []resultRow) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var l structural.Ledger
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatal(err)
	}
	var results []resultRow
	dec := json.NewDecoder(bytes.NewReader(stdout))
	dec.DisallowUnknownFields()
	for dec.More() {
		var r resultRow
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("stdout is not the Result schema: %v", err)
		}
		results = append(results, r)
	}
	return l, results
}

func TestLedgerTasksWriteLedgerAndResultsSeparately(t *testing.T) {
	src := "package p\n\nfunc F_a(p_b, _ int) {}\n"
	path := filepath.Join(t.TempDir(), "ledger.json")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runStructural([]string{"--task", "P3", "--ledger", path, "--id", "fx"}, strings.NewReader(src), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	l, results := readLedger(t, path, stdout.Bytes())
	var streamed []string
	for _, r := range l.Records {
		if r.Cand != nil {
			streamed = append(streamed, r.ID)
		}
	}
	if len(l.Records) != 3 || len(results) != 2 || len(streamed) != 2 {
		t.Fatalf("%d records, %d results", len(l.Records), len(results))
	}
	for i, r := range results {
		if !r.OK || r.Cand != streamed[i] || r.Source.SHA256 != l.Source.SHA256 || r.Source.Bytes != len(src) {
			t.Errorf("result %d = %+v, want the streamed record %s", i, r, streamed[i])
		}
	}
	if strings.Contains(stdout.String(), "status") || stderr.Len() != 0 {
		t.Errorf("ledger metadata leaked to stdout or stderr: %q %q", stdout.String(), stderr.String())
	}
}

func TestP2TargetCertifiesThePackage(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "t.go")
	if err := os.WriteFile(target, []byte(p2Source), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ledger.json")
	var stdout, stderr bytes.Buffer
	args := []string{"--task", "P2", "--ledger", path, "--target", target}
	if code := runStructural(args, strings.NewReader(p2Source), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	l, results := readLedger(t, path, stdout.Bytes())
	if l.Certificate == nil || l.Certificate.Target != "t.go" || len(l.Records) != 2 ||
		l.Records[0].Status != "eligible" || l.Records[1].Status != "excluded_paren_base" || len(results) != 1 {
		t.Errorf("ledger %+v, %d results", l, len(results))
	}

	// Without --target there is no certificate and no eligible record.
	stdout.Reset()
	if code := runStructural([]string{"--task", "P2", "--ledger", path}, strings.NewReader(p2Source), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if l, _ = readLedger(t, path, stdout.Bytes()); l.Certificate != nil || l.Records[0].Status != "unknown_context" {
		t.Errorf("uncertified ledger %+v", l)
	}
}

func TestP2IdentityErrorWritesNothing(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "t.go")
	if err := os.WriteFile(target, []byte(p2Source), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ledger.json")
	for _, tc := range []struct{ target, stdin string }{
		{target, p2Source + "\n"},
		{filepath.Join(dir, "missing.go"), p2Source},
	} {
		var stdout, stderr bytes.Buffer
		args := []string{"--task", "P2", "--ledger", path, "--target", tc.target}
		if code := runStructural(args, strings.NewReader(tc.stdin), &stdout, &stderr); code != 1 || stdout.Len() != 0 {
			t.Errorf("%s: exit %d stdout %q, want 1 and nothing", tc.target, code, stdout.String())
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s: an identity error wrote the ledger", tc.target)
		}
	}
}

func TestLedgerFlagMisuse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	for _, args := range [][]string{
		{"--task", "P2"},
		{"--task", "P3"},
		{"--task", "P1", "--ledger", path},
		{"--task", "P3", "--ledger", path, "--target", "x.go"},
		{"--task", "P1", "--target", "x.go"},
		{"--task", "P2", "--ledger", path, "extra"},
	} {
		var stdout, stderr bytes.Buffer
		if code := runStructural(args, strings.NewReader(selectorSource), &stdout, &stderr); code != 2 ||
			!strings.Contains(stderr.String(), "P2|P3 --ledger PATH") {
			t.Errorf("%v: exit %d, usage %q", args, code, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("%v: misuse wrote results", args)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("misuse wrote a ledger")
	}
}
