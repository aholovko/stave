package astinfo

import (
	"go/parser"
	"go/token"
	"reflect"
	"testing"
)

// topDeclSrc covers each top-level declaration form; only package-block names should survive.
const topDeclSrc = `package p

import "fmt"

const (
	C = 1
	D, E = 2, 3
)

var (
	V, W = 4, 5
	_ = 6
)

type (
	T struct{}
	Alias = T
	_ struct{}
)

// F prints.
func F() {
	const LocalConst = 1
	var localVar int
	type LocalType struct{}
	fmt.Println(V)
}

// M is a method: excluded.
func (T) M() {}

func init() {}
func _() {}
`

func TestTopDeclNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want []string
	}{
		{"package-block names", topDeclSrc, []string{"C", "D", "E", "V", "W", "T", "Alias", "F"}},
		{"empty result", "package p\n", nil},
		{"order and duplicates", "package p\nvar Z, A int\nvar Z int\n", []string{"Z", "A", "Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "src.go", tc.src, ParserMode)
			if err != nil {
				t.Fatal(err)
			}
			if got := TopDeclNames(file); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("TopDeclNames = %v, want %v", got, tc.want)
			}
		})
	}
}
