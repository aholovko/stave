// Package astinfo derives package declarations and source-byte comment spans from Go syntax trees.
package astinfo

import (
	"go/ast"
	"go/parser"
)

// ParserMode is shared by source analysis, package snapshots, and certificate validation.
const ParserMode = parser.ParseComments

// TopDeclNames returns package-block names in declaration order for sibling shadowing checks. Methods, imports,
// init functions, blank identifiers and local declarations are excluded.
func TopDeclNames(file *ast.File) []string {
	var names []string
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && d.Name.Name != "init" && d.Name.Name != "_" {
				names = append(names, d.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if s.Name.Name != "_" {
						names = append(names, s.Name.Name)
					}
				case *ast.ValueSpec:
					for _, name := range s.Names {
						if name.Name != "_" {
							names = append(names, name.Name)
						}
					}
				}
			}
		}
	}
	return names
}
