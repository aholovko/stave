// Package lex wraps go/scanner to expose source tokens with byte offsets.
package lex

import (
	"go/scanner"
	"go/token"
)

type Kind string

const (
	Keyword Kind = "kw"
	Ident   Kind = "ident"
	Int     Kind = "int"
	Float   Kind = "float"
	Imag    Kind = "imag"
	Char    Kind = "char"
	String  Kind = "str"
	Op      Kind = "op"
	Comment Kind = "comment"
)

type Token struct {
	Kind Kind
	Text string
	Off  int
}

func Scan(src []byte) ([]Token, error) {
	fset := token.NewFileSet()
	file := fset.AddFile("src.go", fset.Base(), len(src))
	var errs scanner.ErrorList
	var s scanner.Scanner
	s.Init(file, src, func(pos token.Position, msg string) { errs.Add(pos, msg) }, scanner.ScanComments)
	var out []Token
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.SEMICOLON && lit == "\n" {
			continue // auto-inserted, not present in source
		}
		text := lit
		if text == "" {
			text = tok.String()
		}
		out = append(out, Token{Kind: kindOf(tok), Text: text, Off: file.Offset(pos)})
	}
	if errs.Len() > 0 {
		return nil, errs.Err()
	}
	return out, nil
}

func kindOf(tok token.Token) Kind {
	switch {
	case tok.IsKeyword():
		return Keyword
	case tok == token.IDENT:
		return Ident
	case tok == token.INT:
		return Int
	case tok == token.FLOAT:
		return Float
	case tok == token.IMAG:
		return Imag
	case tok == token.CHAR:
		return Char
	case tok == token.STRING:
		return String
	case tok == token.COMMENT:
		return Comment
	default:
		return Op
	}
}
