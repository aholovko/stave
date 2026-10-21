package advisory

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// Duplicate-content files exercise hash-only response binding.
var request = Request{Schema: Schema, Files: []FileRef{
	{Path: "/pkg/a.go", SHA256: shaA},
	{Path: "/pkg/copy.go", SHA256: shaA},
	{Path: "/pkg/b.go", SHA256: shaB},
}}

// Decode wire JSON so the tests cover serialization semantics too.
func decode(t *testing.T, doc string) Response {
	t.Helper()
	resp, err := Decode(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func response(files, predictions string) string {
	return `{"schema":"stave-advisory-v1","backend":{"id":"fixture","digest":"d","detail":"full"},"classes":["DC"],` +
		`"files":[` + files + `],"predictions":[` + predictions + `]}`
}

func TestDecodeReadsTheWireExample(t *testing.T) {
	resp := decode(t, `{
	  "schema":"stave-advisory-v1",
	  "backend":{"id":"test-model","digest":"artifact file sha256","detail":"full"},
	  "classes":["DC"],
	  "files":[{"path":"/absolute/demo.go","sha256":"`+shaA+`","status":"checked","reason":null}],
	  "predictions":[{
	    "path":"/absolute/demo.go","class":"DC","sha256":"`+shaA+`",
	    "span":{"start":20,"end":60},"coverage":"checked","advice":true,"score":0.8,"reason":null
	  }]
	}`)

	if resp.Backend.ID != "test-model" || resp.Backend.Digest != "artifact file sha256" || resp.Backend.Detail != "full" {
		t.Errorf("backend = %+v", resp.Backend)
	}
	p := resp.Predictions[0]
	if p.Span.Start != 20 || p.Span.End != 60 || p.Advice == nil || !*p.Advice || p.Score == nil || *p.Score != 0.8 || p.Reason != nil {
		t.Errorf("prediction = %+v", p)
	}
	if f := resp.Files[0]; f.Status != "checked" || f.Reason != nil || f.Path != "/absolute/demo.go" {
		t.Errorf("file record = %+v", f)
	}
}

func TestDecodeRejectsAnythingButOneJSONObject(t *testing.T) {
	for name, doc := range map[string]string{
		"garbage":       "not json",
		"empty":         "",
		"trailing data": response("", "") + " {}",
		"wrong type":    `{"schema":"stave-advisory-v1","backend":"a5"}`,
	} {
		if _, err := Decode(strings.NewReader(doc)); err == nil {
			t.Errorf("%s: decoded without an error", name)
		}
	}
}

func TestTheRequestWireFormIsExact(t *testing.T) {
	got, err := json.Marshal(Request{Schema: Schema, Files: []FileRef{{Path: "/absolute/demo.go", SHA256: shaA}}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema":"stave-advisory-v1","files":[{"path":"/absolute/demo.go","sha256":"` + shaA + `"}]}`
	if string(got) != want {
		t.Errorf("request = %s\nwant      %s", got, want)
	}
}

func TestARecordWithoutAPathAppliesToEveryRequestedFileWithThatHash(t *testing.T) {
	resp := decode(t, response(
		`{"sha256":"`+shaA+`","status":"checked","reason":null}`,
		`{"class":"DC","sha256":"`+shaA+`","span":{"start":1,"end":4},"coverage":"checked","advice":true,"score":null,"reason":null}`,
	))

	bound, err := Resolve(request, resp)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/pkg/a.go", "/pkg/copy.go"} {
		r := bound[path]
		if r.Record == nil || r.Record.Status != StatusChecked || len(r.Predictions) != 1 {
			t.Errorf("%s: bound = %+v, want the pathless record and its prediction", path, r)
		}
	}
	if r := bound["/pkg/b.go"]; r.Record != nil || len(r.Predictions) != 0 {
		t.Errorf("/pkg/b.go: bound = %+v, want no record: the backend said nothing about this source", r)
	}
}

func TestARecordWithAPathBindsOnlyThatPathAndHash(t *testing.T) {
	resp := decode(t, response(
		`{"path":"/pkg/a.go","sha256":"`+shaA+`","status":"checked","reason":null}`,
		`{"path":"/pkg/a.go","class":"DC","sha256":"`+shaA+`","span":{"start":1,"end":4},"coverage":"checked","advice":false,"score":0.1,"reason":null}`,
	))

	bound, err := Resolve(request, resp)
	if err != nil {
		t.Fatal(err)
	}
	if r := bound["/pkg/a.go"]; r.Record == nil || len(r.Predictions) != 1 {
		t.Errorf("/pkg/a.go: bound = %+v", r)
	}
	if r := bound["/pkg/copy.go"]; r.Record != nil || len(r.Predictions) != 0 {
		t.Errorf("/pkg/copy.go: bound = %+v, want nothing: the record names another path", r)
	}
}

func TestIdenticalDuplicatesCollapseButContradictionsAreRejected(t *testing.T) {
	file := `{"sha256":"` + shaA + `","status":"checked","reason":null}`
	prediction := `{"class":"DC","sha256":"` + shaA + `","span":{"start":1,"end":4},"coverage":"checked","advice":true,"score":null,"reason":null}`

	bound, err := Resolve(request, decode(t, response(file+","+file, prediction+","+prediction)))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(bound["/pkg/a.go"].Predictions); got != 1 {
		t.Errorf("identical duplicates bound %d predictions, want 1", got)
	}

	contradiction := strings.Replace(prediction, `"advice":true`, `"advice":false`, 1)
	if _, err := Resolve(request, decode(t, response(file, prediction+","+contradiction))); err == nil {
		t.Error("two answers for one candidate must be rejected")
	}
	otherFile := `{"path":"/pkg/a.go","sha256":"` + shaA + `","status":"error","reason":"stale"}`
	if _, err := Resolve(request, decode(t, response(file+","+otherFile, ""))); err == nil {
		t.Error("two statuses for one file must be rejected")
	}
}

func TestResolveRejectsWhatTheContractForbids(t *testing.T) {
	checked := `{"sha256":"` + shaA + `","status":"checked","reason":null}`
	prediction := func(fields string) string {
		return `{"class":"DC","sha256":"` + shaA + `","span":{"start":1,"end":4},` + fields + `}`
	}
	ok := `"coverage":"checked","advice":true,"score":null,"reason":null`

	for name, doc := range map[string]string{
		"schema":                         strings.Replace(response(checked, ""), "stave-advisory-v1", "stave-advisory-v0", 1),
		"no backend id":                  strings.Replace(response(checked, ""), `"id":"fixture"`, `"id":""`, 1),
		"a legacy class is declared":     strings.Replace(response(checked, ""), `["DC"]`, `["DC","A5"]`, 1),
		"unrequested file hash":          response(`{"sha256":"`+shaC+`","status":"checked","reason":null}`, ""),
		"unrequested prediction hash":    response(checked, strings.Replace(prediction(ok), shaA, shaC, 1)),
		"stale path and hash pair":       response(`{"path":"/pkg/b.go","sha256":"`+shaA+`","status":"checked","reason":null}`, ""),
		"unrequested path":               response(`{"path":"/elsewhere.go","sha256":"`+shaA+`","status":"checked","reason":null}`, ""),
		"file status":                    response(`{"sha256":"`+shaA+`","status":"fine","reason":null}`, ""),
		"file error without a reason":    response(`{"sha256":"`+shaA+`","status":"error","reason":null}`, ""),
		"class the backend never serves": response(checked, strings.Replace(prediction(ok), `"DC"`, `"RP"`, 1)),
		"empty span":                     response(checked, strings.Replace(prediction(ok), `"end":4`, `"end":1`, 1)),
		"negative span":                  response(checked, strings.Replace(prediction(ok), `"start":1`, `"start":-1`, 1)),
		"prediction coverage":            response(checked, prediction(`"coverage":"maybe","advice":true,"score":null,"reason":null`)),
		"checked without advice":         response(checked, prediction(`"coverage":"checked","advice":null,"score":null,"reason":null`)),
		"unsupported with advice":        response(checked, prediction(`"coverage":"unsupported","advice":true,"score":null,"reason":"too long"`)),
		"unsupported with a score":       response(checked, prediction(`"coverage":"unsupported","advice":null,"score":0.5,"reason":"too long"`)),
		"unsupported without a reason":   response(checked, prediction(`"coverage":"unsupported","advice":null,"score":null,"reason":null`)),
		"prediction without its file":    response("", prediction(ok)),
		"prediction for a failed file":   response(`{"sha256":"`+shaA+`","status":"error","reason":"stale"}`, prediction(ok)),
	} {
		resp, err := Decode(strings.NewReader(doc))
		if err != nil {
			t.Errorf("%s: fixture does not decode: %v", name, err)
			continue
		}
		if _, err := Resolve(request, resp); err == nil {
			t.Errorf("%s: resolved without an error", name)
		}
	}
}

func six(files, predictions string) string {
	return `{"schema":"stave-advisory-v1","backend":{"id":"six","digest":"d","detail":"B"},` +
		`"classes":["P1","P2","P3","TF","DC","RP"],"files":[` + files + `],"predictions":[` + predictions + `]}`
}

func TestASixClassRecordCarriesItsClassesContextAndWithheldCounts(t *testing.T) {
	resp := decode(t, six(
		`{"path":"/pkg/a.go","sha256":"`+shaA+`","status":"checked","reason":null,`+
			`"classes":["P1","P2","DC"],"context":"ctx","withheld":{"DC":{"selector withheld: no_prose":2}}}`,
		`{"path":"/pkg/a.go","class":"P1","sha256":"`+shaA+`","span":{"start":1,"end":4},"coverage":"checked","advice":true,"score":-0.5,"reason":null}`,
	))

	bound, err := Resolve(request, resp)
	if err != nil {
		t.Fatal(err)
	}
	rec := bound["/pkg/a.go"].Record
	if rec == nil || strings.Join(rec.Classes, ",") != "P1,P2,DC" || rec.Context != "ctx" || rec.Withheld["DC"]["selector withheld: no_prose"] != 2 {
		t.Errorf("record = %+v", rec)
	}

	minimal := decode(t, response(`{"sha256":"`+shaA+`","status":"checked","reason":null}`, ""))
	bound, err = Resolve(request, minimal)
	if err != nil {
		t.Fatal(err)
	}
	if rec := bound["/pkg/a.go"].Record; rec.Classes != nil || rec.Context != "" || rec.Withheld != nil {
		t.Errorf("a minimal record = %+v, want no classes, context or withheld counts", rec)
	}
}

// An empty class list means no coverage; nil means all declared classes. Re-encoding must preserve the distinction.
func TestAnEmptyClassListSurvivesARoundTripAsNoClasses(t *testing.T) {
	for doc, want := range map[string]string{
		`{"sha256":"` + shaA + `","status":"checked","reason":null,"classes":[]}`: `"classes":[]`,
		`{"sha256":"` + shaA + `","status":"checked","reason":null}`:              "",
	} {
		resp := decode(t, six(doc, ""))
		body, err := json.Marshal(resp)
		if err != nil {
			t.Fatal(err)
		}
		again := decode(t, string(body))
		rec := again.Files[0]
		if want == "" {
			if strings.Contains(string(body), `"classes":[]`) || rec.Classes != nil || !rec.Checks("P1") {
				t.Errorf("minimal record re-encoded as %s: classes %v", body, rec.Classes)
			}
			continue
		}
		if !strings.Contains(string(body), want) || rec.Classes == nil || rec.Checks("P1") {
			t.Errorf("empty class list re-encoded as %s: classes %v, checks P1 %v", body, rec.Classes, rec.Checks("P1"))
		}
	}
}

func TestResolveRejectsSixClassRecordsThatContradictTheirClasses(t *testing.T) {
	record := func(extra string) string {
		return `{"path":"/pkg/a.go","sha256":"` + shaA + `","status":"checked","reason":null` + extra + `}`
	}
	prediction := `{"path":"/pkg/a.go","class":"TF","sha256":"` + shaA + `","span":{"start":1,"end":4},"coverage":"checked","advice":true,"score":0.5,"reason":null}`
	for name, doc := range map[string]string{
		"a record class the backend does not declare": six(record(`,"classes":["P1","A5"]`), ""),
		"a prediction of a class its record omits":    six(record(`,"classes":["P1"]`), prediction),
		"withheld counts of a class the record omits": six(record(`,"classes":["P1"],"withheld":{"DC":{"no_prose":1}}`), ""),
		"withheld counts of an undeclared class":      six(record(`,"withheld":{"A5":{"x":1}}`), ""),
		"a withheld count that is not positive":       six(record(`,"withheld":{"DC":{"no_prose":0}}`), ""),
		"a withheld count without a reason":           six(record(`,"withheld":{"DC":{"":1}}`), ""),
		"a failed record that names classes":          six(`{"path":"/pkg/a.go","sha256":"`+shaA+`","status":"error","reason":"x","classes":["P1"]}`, ""),
		"pathless records that disagree on classes": six(
			`{"sha256":"`+shaA+`","status":"checked","reason":null,"classes":["P1"]},`+
				`{"sha256":"`+shaA+`","status":"checked","reason":null,"classes":["P2"]}`, ""),
		"pathless records that disagree on context": six(
			`{"sha256":"`+shaA+`","status":"checked","reason":null,"context":"a"},`+
				`{"sha256":"`+shaA+`","status":"checked","reason":null,"context":"b"}`, ""),
	} {
		resp, err := Decode(strings.NewReader(doc))
		if err != nil {
			t.Errorf("%s: fixture does not decode: %v", name, err)
			continue
		}
		if _, err := Resolve(request, resp); err == nil {
			t.Errorf("%s: resolved without an error", name)
		}
	}
}

func TestAnUnsupportedCandidateIsCarriedNotRejected(t *testing.T) {
	resp := decode(t, response(
		`{"sha256":"`+shaB+`","status":"checked","reason":null}`,
		`{"class":"DC","sha256":"`+shaB+`","span":{"start":5,"end":9},"coverage":"unsupported","advice":null,"score":null,"reason":"candidate exceeds the window"}`,
	))

	bound, err := Resolve(request, resp)
	if err != nil {
		t.Fatal(err)
	}
	p := bound["/pkg/b.go"].Predictions
	if len(p) != 1 || p[0].Coverage != StatusUnsupported || p[0].Reason == nil || *p[0].Reason != "candidate exceeds the window" {
		t.Errorf("predictions = %+v", p)
	}
}
