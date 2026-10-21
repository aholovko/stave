package instance

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func shaOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// naiveDigest joins fields with LF to demonstrate the ambiguity that NUL framing prevents.
func naiveDigest(rows []Row) string {
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(r.Path)
		b.WriteString("\n")
		if r.SHA != "" {
			b.WriteString(r.SHA)
		} else {
			b.WriteString("STATUS:" + r.Status)
		}
		b.WriteString("\n")
	}
	return shaOf(b.String())
}

// Paths may contain LF, so LF-separated fields can give distinct file sets the same context digest.
func TestDigestFramingIsInjectiveAcrossAnEmbeddedNewline(t *testing.T) {
	s1, s2 := shaOf("one"), shaOf("two")

	split := []Row{{Path: "a", SHA: s1}, {Path: "b", SHA: s2}}
	// This path contains the LF-separated representation of the first row.
	fused := []Row{{Path: "a\n" + s1 + "\nb", SHA: s2}}

	if naiveDigest(split) != naiveDigest(fused) {
		t.Fatal("the two row sets must collide under LF framing, or this test proves nothing about NUL framing")
	}

	splitCtx, err := NewPackageContext(split, true)
	if err != nil {
		t.Fatal(err)
	}
	fusedCtx, err := NewPackageContext(fused, true)
	if err != nil {
		t.Fatal(err)
	}
	if splitCtx.Digest() == fusedCtx.Digest() {
		t.Fatal("the specified serialization collided on row sets that differ; the NUL separator is not doing its work")
	}
}

// This pair collides when the field separator is omitted; the LF-separator case alone does not catch that
// ambiguity.
func TestDigestFramingSurvivesRemovingTheSeparatorEntirely(t *testing.T) {
	h := shaOf("payload")

	split := []Row{{Path: "p1", Status: "X"}, {Path: "q", SHA: h}}
	fused := []Row{{Path: "p1STATUS:X\nq", SHA: h}}

	noSep := func(rows []Row) string {
		var b strings.Builder
		for _, r := range rows {
			b.WriteString(r.Path)
			if r.SHA != "" {
				b.WriteString(r.SHA)
			} else {
				b.WriteString("STATUS:" + r.Status)
			}
			b.WriteString("\n")
		}
		return shaOf(b.String())
	}
	if noSep(split) != noSep(fused) {
		t.Fatal("the two row sets must collide with the separator removed, or this test proves nothing")
	}

	splitCtx, err := NewPackageContext(split, true)
	if err != nil {
		t.Fatal(err)
	}
	fusedCtx, err := NewPackageContext(fused, true)
	if err != nil {
		t.Fatal(err)
	}
	if splitCtx.Digest() == fusedCtx.Digest() {
		t.Fatal("the specified serialization collided; the NUL separator is absent or not framing both fields")
	}
}

func TestDigestIsOrderIndependentAndContentSensitive(t *testing.T) {
	a := Row{Path: "a.go", SHA: shaOf("one")}
	b := Row{Path: "b.go", SHA: shaOf("two")}

	forward, err := NewPackageContext([]Row{a, b}, true)
	if err != nil {
		t.Fatal(err)
	}
	reverse, err := NewPackageContext([]Row{b, a}, true)
	if err != nil {
		t.Fatal(err)
	}
	if forward.Digest() != reverse.Digest() {
		t.Fatal("the digest must sort rows by path; it depends on caller order")
	}

	changed, err := NewPackageContext([]Row{a, {Path: "b.go", SHA: shaOf("three")}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if forward.Digest() == changed.Digest() {
		t.Fatal("a changed sibling hash must change the digest")
	}
}

// Unknown content must remain distinct from hashed content.
func TestStatusRowsAreNotConfusableWithHashes(t *testing.T) {
	statusCtx, err := NewPackageContext([]Row{{Path: "a.go", Status: "oversized"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	hashedCtx, err := NewPackageContext([]Row{{Path: "a.go", SHA: shaOf("oversized")}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if statusCtx.Digest() == hashedCtx.Digest() {
		t.Fatal("a status row and a hashed row produced the same digest")
	}
	other, err := NewPackageContext([]Row{{Path: "a.go", Status: "unreadable"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if statusCtx.Digest() == other.Digest() {
		t.Fatal("two different statuses produced the same digest")
	}

	// A status that looks like a hash still needs the STATUS prefix to remain distinct from known content.
	h := shaOf("collide")
	asStatus, err := NewPackageContext([]Row{{Path: "a.go", Status: h}}, false)
	if err != nil {
		t.Fatal(err)
	}
	asHash, err := NewPackageContext([]Row{{Path: "a.go", SHA: h}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if asStatus.Digest() == asHash.Digest() {
		t.Fatal("a status spelling a valid hash collided with that hash; the STATUS: prefix is not doing its work")
	}
}

// Readable, parseable siblings contribute declarations even when an exclusion status keeps them out of analysis.
func TestAParseableSiblingCarriesItsHashItsStatusAndItsDeclarations(t *testing.T) {
	h := shaOf("generated body")
	both := Row{Path: "gen.go", SHA: h, Status: "generated", TopDecls: []string{"Foo"}}

	ctx, err := NewPackageContext([]Row{both}, true)
	if err != nil {
		t.Fatalf("a parseable non-ok sibling was refused: %v", err)
	}
	if !ctx.Declares("Foo") {
		t.Error("the sibling's declaration did not reach the declaration union")
	}
	got := ctx.Rows()[0]
	if got.SHA != h || got.Status != "generated" {
		t.Errorf("the row round-tripped as SHA %q status %q", got.SHA, got.Status)
	}

	// Known content determines identity, so changing its exclusion status alone leaves the digest unchanged.
	hashOnly, err := NewPackageContext([]Row{{Path: "gen.go", SHA: h}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Digest() != hashOnly.Digest() {
		t.Error("the digest read the status of a row that has a hash; it must read the hash whenever one is present")
	}
}

func TestPackageContextRejectsMalformedRows(t *testing.T) {
	good := shaOf("x")
	for _, tc := range []struct {
		name   string
		rows   []Row
		reject string
	}{
		{"neither SHA nor status", []Row{{Path: "a.go"}}, "at least one of SHA and Status"},
		{"empty path", []Row{{Path: "", SHA: good}}, "empty path"},
		{"path containing NUL", []Row{{Path: "a\x00b.go", SHA: good}}, "path contains NUL"},
		{"status containing LF", []Row{{Path: "a.go", Status: "over\nsized"}}, "contains NUL or LF"},
		{"short SHA", []Row{{Path: "a.go", SHA: "abc"}}, "want 64"},
		{"uppercase SHA", []Row{{Path: "a.go", SHA: strings.ToUpper(good)}}, "lowercase hex"},
		{"duplicate paths", []Row{{Path: "a.go", SHA: good}, {Path: "a.go", SHA: good}}, "duplicate path"},
		{
			"declarations without a hash",
			[]Row{{Path: "a.go", Status: "unparsable", TopDecls: []string{"Foo"}}},
			"declarations without a hash",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewPackageContext(tc.rows, true)
			if err == nil {
				t.Fatalf("rows %v were accepted; they must be rejected for %q", tc.rows, tc.reject)
			}
			if !strings.Contains(err.Error(), tc.reject) {
				t.Fatalf("rejected with %q, want a rejection naming %q", err, tc.reject)
			}
		})
	}

	// Accept the nearby valid cases so rejecting every row cannot satisfy the test.
	for _, rows := range [][]Row{
		{{Path: "a.go", SHA: good}},
		{{Path: "a.go", Status: "oversized"}},
		{{Path: "a\nb.go", SHA: good}},
		{{Path: "a.go", SHA: good, TopDecls: []string{"Foo"}}},
		{{Path: "a.go", SHA: good, Status: "generated"}},
		{{Path: "a.go", SHA: good, Status: "generated", TopDecls: []string{"Foo"}}},
		// A parse error leaves the content hash known while declarations remain unknown.
		{{Path: "a.go", SHA: good, Status: "parse-error"}},
	} {
		if _, err := NewPackageContext(rows, true); err != nil {
			t.Fatalf("rows %v must be accepted, got %v", rows, err)
		}
	}
}

func TestPkgFlagInventoryIsTheFrozenEight(t *testing.T) {
	if got := len(PkgFlagNames); got != 8 {
		t.Fatalf("package context has %d flags, want 8: one context-completeness, two conflict, five shadowing", got)
	}
	seen := map[string]bool{}
	for _, n := range PkgFlagNames {
		if seen[n] {
			t.Fatalf("duplicate flag name %q", n)
		}
		seen[n] = true
	}
}
