package calls

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// Anything a call remembers belongs on the Subsystem, where its lifetime is
// the subsystem's. A package-level cache is never reaped and is shared by
// every subsystem in the process, so in one test binary it answers a later
// test from an earlier one's rooms.
//
// Package-level values that cannot hold state are still allowed: the event
// types, which mautrix requires as vars because event.Type is a struct, and
// the sentinel errors callers compare with errors.Is.
func TestNoPackageLevelMutableState(t *testing.T) {
	dir := filepath.Join(moduleRoot(t), "pkg", "calls")
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	if len(pkgs) == 0 {
		t.Fatalf("no packages parsed in %s", dir)
	}
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.VAR {
					continue
				}
				for _, spec := range gen.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range value.Names {
						if immutablePackageValue(value, i) {
							continue
						}
						t.Errorf("%s: package-level var %s holds state; put it on Subsystem",
							filepath.Base(path), name.Name)
					}
				}
			}
		}
	}
}

// immutablePackageValue reports whether the i-th name of a package-level var
// is one of the two shapes that cannot accumulate anything: an event.Type
// literal or an errors.New sentinel.
func immutablePackageValue(spec *ast.ValueSpec, i int) bool {
	if i >= len(spec.Values) {
		return false
	}
	switch v := spec.Values[i].(type) {
	case *ast.CompositeLit:
		sel, ok := v.Type.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && pkg.Name == "event" && sel.Sel.Name == "Type"
	case *ast.CallExpr:
		sel, ok := v.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && pkg.Name == "errors" && sel.Sel.Name == "New"
	default:
		return false
	}
}
