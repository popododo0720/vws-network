package network

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnnotationKeyScheme(t *testing.T) {
	if annotationID != "vws.popododo0720.io/id" {
		t.Fatalf("annotationID %q", annotationID)
	}
}

func TestNoAnnotationKeyOutsideScheme(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		tree, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(tree, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v := strings.Trim(lit.Value, "\"")
			if strings.HasPrefix(v, "vws.") && v != annotationPrefix && !strings.HasPrefix(v, annotationPrefix) {
				t.Errorf("%s: key %q is outside %q", fset.Position(lit.Pos()), v, annotationPrefix)
			}
			return true
		})
	}
}
