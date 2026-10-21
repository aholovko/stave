// Package tfsentry independently scans for Error, Errorf, Fatal and Fatalf selectors
// so the TF admission pass cannot silently miss them. A hit alone does not establish
// a testing binding or an eligible call.
package tfsentry

import (
	"fmt"
	"go/scanner"
	"go/token"
)

var methods = map[string]bool{"Error": true, "Errorf": true, "Fatal": true, "Fatalf": true}

// Hit is one selector token pair. Period is the offset of the PERIOD, and [Offset, End) is the method identifier. Form
// is direct_call when the next token is a left parenthesis, and other otherwise (including end of input).
type Hit struct {
	Period int    `json:"period"`
	Offset int    `json:"offset"`
	End    int    `json:"end"`
	Method string `json:"method"`
	Form   string `json:"form"`
}

// Result holds selector hits in source order and scanner errors as "offset: message".
// Scanning continues after errors so coverage remains visible.
type Result struct {
	Hits   []Hit    `json:"hits"`
	Total  int      `json:"total"`
	Errors []string `json:"errors"`
}

func Scan(src []byte) Result {
	r := Result{Hits: []Hit{}, Errors: []string{}}
	file := token.NewFileSet().AddFile("", -1, len(src))
	var s scanner.Scanner
	s.Init(file, src, func(pos token.Position, msg string) {
		r.Errors = append(r.Errors, fmt.Sprintf("%d: %s", pos.Offset, msg))
	}, 0)
	period := -1
	open := false // the latest hit awaits its form
	for {
		pos, tok, lit := s.Scan()
		if open {
			r.Hits[len(r.Hits)-1].Form = "other"
			if tok == token.LPAREN {
				r.Hits[len(r.Hits)-1].Form = "direct_call"
			}
			open = false
		}
		if tok == token.EOF {
			break
		}
		at := file.Offset(pos)
		if tok == token.IDENT && period >= 0 && methods[lit] {
			r.Hits = append(r.Hits, Hit{Period: period, Offset: at, End: at + len(lit), Method: lit})
			open = true
		}
		period = -1
		if tok == token.PERIOD {
			period = at
		}
	}
	r.Total = len(r.Hits)
	return r
}
