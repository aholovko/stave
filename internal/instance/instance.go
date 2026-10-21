package instance

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"

	"github.com/aholovko/stave/internal/astinfo"
	"github.com/aholovko/stave/internal/lex"
)

// ParserMode is shared with astinfo so analysis and package snapshots retain the same comments and positions.
const ParserMode = astinfo.ParserMode

// Instance holds checked source bytes, their parsed data, and package flags for analysis.
type Instance struct {
	src   []byte
	file  *ast.File
	fset  *token.FileSet
	toks  []lex.Token
	flags []bool
}

// New checks the source hash against its manifest row and derives all parsed data from those same bytes, so
// callers cannot supply stale positions or lexical records.
func New(path string, ctx *PackageContext, src []byte) (*Instance, error) {
	if ctx == nil {
		return nil, fmt.Errorf("instance %s: nil package context", path)
	}

	sum := sha256.Sum256(src)
	sourceSHA := hex.EncodeToString(sum[:])

	var distinguished *Row
	for _, r := range ctx.Rows() {
		if r.Path == path {
			row := r
			distinguished = &row
			break
		}
	}
	if distinguished == nil {
		return nil, fmt.Errorf("instance %s: no distinguished manifest row for the analysed file", path)
	}
	if distinguished.SHA != sourceSHA {
		return nil, fmt.Errorf(
			"instance %s: source hash %s does not match its manifest row %q",
			path, sourceSHA[:12], distinguished.SHA)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, ParserMode)
	if err != nil {
		return nil, fmt.Errorf("instance %s: parse failed: %w", path, err)
	}
	toks, err := lex.Scan(src)
	if err != nil {
		return nil, fmt.Errorf("instance %s: lexical derivation failed: %w", path, err)
	}

	return &Instance{
		src:   src,
		file:  file,
		fset:  fset,
		toks:  toks,
		flags: ctx.pkgFlags(),
	}, nil
}

// Source returns a copy so callers cannot change the bytes used to derive parsed data.
func (i *Instance) Source() []byte {
	out := make([]byte, len(i.src))
	copy(out, i.src)
	return out
}

func (i *Instance) File() *ast.File { return i.file }

// Offset converts an AST position to a byte offset. Invalid positions return -1 so they cannot be mistaken for the
// start of the source.
func (i *Instance) Offset(p token.Pos) int {
	if !p.IsValid() {
		return -1
	}
	return i.fset.Position(p).Offset
}

// Tokens returns the derived lexical records.
func (i *Instance) Tokens() []lex.Token {
	out := make([]lex.Token, len(i.toks))
	copy(out, i.toks)
	return out
}

// PkgFlags returns package flags in PkgFlagNames order.
func (i *Instance) PkgFlags() []bool {
	out := make([]bool, len(i.flags))
	copy(out, i.flags)
	return out
}

// PkgFlag rejects unknown names so a misspelled flag cannot silently read as false.
func (i *Instance) PkgFlag(name string) (bool, error) {
	for k, n := range PkgFlagNames {
		if n == name {
			return i.flags[k], nil
		}
	}
	return false, fmt.Errorf("package context has no flag %q", name)
}
