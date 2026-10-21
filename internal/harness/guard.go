package harness

import (
	"bytes"
	"fmt"
	"go/token"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aholovko/stave/internal/diag"
	"github.com/aholovko/stave/internal/span"
)

// adapt verifies analyzer spans against the original source before reporting them.
// One invalid span withholds its whole class so partial results cannot look complete.
func adapt(src []byte, occurrences []span.Occurrence) (findings map[string][]diag.Span, failed map[string]string) {
	findings, failed = map[string][]diag.Span{}, map[string]string{}
	for _, o := range occurrences {
		class := o.Class
		s, err := guard(class, src, diag.Span{Start: o.Span.Start, End: o.Span.End})
		if err != nil {
			if failed[class] == "" {
				failed[class] = fmt.Sprintf("%s span %s contradicts the original bytes: %v", class, o.Span, err)
			}
			continue
		}
		findings[class] = append(findings[class], s)
	}
	for class := range failed {
		delete(findings, class)
	}
	return findings, failed
}

// guard checks that each span covers the token expected for its class.
func guard(class string, src []byte, s diag.Span) (diag.Span, error) {
	if s.Start < 0 || s.Start >= s.End || s.End > len(src) {
		return s, fmt.Errorf("not a nonempty range within %d bytes", len(src))
	}
	if class == "P2" {
		return guardLiteral(src, s)
	}

	text := string(src[s.Start:s.End])
	if !wholeWord(src, s) {
		return s, fmt.Errorf("%q is not a whole word", text)
	}
	ok := false
	switch class {
	case "P1":
		ok = text == "else"
	case "P3":
		ok = token.IsIdentifier(text) && strings.Contains(text, "_")
	}
	if !ok {
		return s, fmt.Errorf("%q is not what %s anchors on", text, class)
	}
	return s, nil
}

// Derive the literal end from source bytes because scanner values omit carriage
// returns in raw strings. No other endpoint discrepancy is allowed.
func guardLiteral(src []byte, s diag.Span) (diag.Span, error) {
	end, err := literalEnd(src, s.Start)
	if err != nil {
		return s, err
	}
	dropped := 0
	if src[s.Start] == '`' {
		dropped = bytes.Count(src[s.Start:end], []byte{'\r'})
	}
	if s.End > end || end-s.End > dropped {
		return s, fmt.Errorf("the literal ends at %d, not %d", end, s.End)
	}
	return diag.Span{Start: s.Start, End: end}, nil
}

// literalEnd returns the offset just past the string literal that opens at start. It reads only the original bytes.
func literalEnd(src []byte, start int) (int, error) {
	switch src[start] {
	case '`':
		i := bytes.IndexByte(src[start+1:], '`')
		if i < 0 {
			return 0, fmt.Errorf("raw string literal at %d is never closed", start)
		}
		return start + 1 + i + 1, nil
	case '"':
		for i := start + 1; i < len(src); i++ {
			switch src[i] {
			case '\\':
				i++
			case '\n':
				return 0, fmt.Errorf("string literal at %d is never closed", start)
			case '"':
				if _, err := strconv.Unquote(string(src[start : i+1])); err != nil {
					return 0, fmt.Errorf("string literal at %d: %v", start, err)
				}
				return i + 1, nil
			}
		}
		return 0, fmt.Errorf("string literal at %d is never closed", start)
	}
	return 0, fmt.Errorf("no string literal opens at %d", start)
}

// wholeWord reports whether s is delimited on both sides. A span that starts or stops inside an identifier is not the
// token that its class anchors on.
func wholeWord(src []byte, s diag.Span) bool {
	before, _ := utf8.DecodeLastRune(src[:s.Start])
	after, _ := utf8.DecodeRune(src[s.End:])
	return !identRune(before) && !identRune(after)
}

func identRune(r rune) bool {
	return r == '_' || (r != utf8.RuneError && (unicode.IsLetter(r) || unicode.IsDigit(r)))
}
