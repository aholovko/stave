package diag

import (
	"fmt"
	"strings"
)

// NotJudged gives the CLI and language server the same wording for coverage gaps.
// Adjacent classes share a line only when their full explanations match.
func NotJudged(coverage []Coverage) []string {
	var lines, classes []string
	var last string
	flush := func() {
		if len(classes) > 0 {
			lines = append(lines, strings.Join(classes, " ")+" "+last)
		}
		classes = nil
	}
	for _, c := range coverage {
		if c.Status == StatusChecked {
			continue
		}
		why := c.Origin + " " + c.Status + ": " + c.Reason
		var items []string
		for _, item := range c.Items {
			if item.Span != nil {
				items = append(items, fmt.Sprintf("bytes [%d,%d): %s", item.Span.Start, item.Span.End, item.Reason))
			} else {
				items = append(items, item.Reason)
			}
		}
		if len(items) > 0 {
			why += " [" + strings.Join(items, "; ") + "]"
		}
		if why != last {
			flush()
		}
		classes, last = append(classes, c.Class), why
	}
	flush()
	return lines
}

// Candidates formats a class's candidate count; advised is its finding count.
// An absent count returns "" so missing coverage cannot be mistaken for zero candidates.
func Candidates(c Coverage, advised int) string {
	if c.Candidates == nil {
		return ""
	}
	n := *c.Candidates
	label := c.Class
	if n == 0 {
		return label + ": 0 candidates reported"
	}
	noun := "candidates"
	if n == 1 {
		noun = "candidate"
	}
	line := fmt.Sprintf("%s: %d %s reported, %d advised", label, n, noun, advised)
	if len(c.Items) > 0 {
		line += fmt.Sprintf(", %d not judged", len(c.Items))
		if c.Status == StatusError {
			line += " (error)"
		}
	}
	return line
}
