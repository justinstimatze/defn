// Package ingest loads Go source code from disk, parses it with go/ast,
// extracts definitions, and stores them in the defn database.
package ingest

import (
	"fmt"
	"go/ast"
	"go/token"
	"strings"

	"github.com/justinstimatze/defn/internal/store"
	"golang.org/x/tools/go/packages"
)

// Ingest loads a Go module from modulePath and stores all definitions
// into the database. modulePath should be a directory containing go.mod.
func Ingest(db *store.DB, modulePath string) error {
	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedImports |
			packages.NeedDeps,
		Dir: modulePath,
	}

	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return fmt.Errorf("load packages: %w", err)
	}

	// Check for load errors.
	var errs []string
	packages.Visit(pkgs, nil, func(pkg *packages.Package) {
		for _, e := range pkg.Errors {
			errs = append(errs, e.Error())
		}
	})
	if len(errs) > 0 {
		return fmt.Errorf("package errors:\n%s", strings.Join(errs, "\n"))
	}

	for _, pkg := range pkgs {
		if err := ingestPackage(db, pkg); err != nil {
			return fmt.Errorf("ingest %s: %w", pkg.PkgPath, err)
		}
	}

	return nil
}

func ingestPackage(db *store.DB, pkg *packages.Package) error {
	mod, err := db.EnsureModule(pkg.PkgPath, pkg.Name)
	if err != nil {
		return err
	}

	for _, file := range pkg.Syntax {
		if err := ingestFile(db, pkg, mod, file); err != nil {
			return err
		}
	}
	return nil
}

func ingestFile(db *store.DB, pkg *packages.Package, mod *store.Module, file *ast.File) error {
	fset := pkg.Fset

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if err := ingestFunc(db, fset, mod, file, d); err != nil {
				return err
			}
		case *ast.GenDecl:
			if err := ingestGenDecl(db, fset, mod, file, d); err != nil {
				return err
			}
		}
	}
	return nil
}

func ingestFunc(db *store.DB, fset *token.FileSet, mod *store.Module, file *ast.File, fn *ast.FuncDecl) error {
	start := fset.Position(fn.Pos())
	end := fset.Position(fn.End())

	var receiver string
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		receiver = typeString(fn.Recv.List[0].Type)
	}

	kind := "function"
	if receiver != "" {
		kind = "method"
	}

	body := renderNode(fset, fn)
	sig := renderSignature(fset, fn)
	doc := fn.Doc.Text()

	def := &store.Definition{
		ModuleID:  mod.ID,
		Name:      fn.Name.Name,
		Kind:      kind,
		Exported:  fn.Name.IsExported(),
		Receiver:  receiver,
		Signature: sig,
		Body:      body,
		Doc:       doc,
		StartLine: start.Line,
		EndLine:   end.Line,
	}

	_, err := db.UpsertDefinition(def)
	return err
}

func ingestGenDecl(db *store.DB, fset *token.FileSet, mod *store.Module, file *ast.File, gd *ast.GenDecl) error {
	for _, spec := range gd.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			start := fset.Position(gd.Pos())
			end := fset.Position(gd.End())

			kind := "type"
			if _, ok := s.Type.(*ast.InterfaceType); ok {
				kind = "interface"
			}

			body := renderNode(fset, gd)
			doc := gd.Doc.Text()
			if doc == "" {
				doc = s.Doc.Text()
			}

			def := &store.Definition{
				ModuleID:  mod.ID,
				Name:      s.Name.Name,
				Kind:      kind,
				Exported:  s.Name.IsExported(),
				Signature: fmt.Sprintf("type %s", s.Name.Name),
				Body:      body,
				Doc:       doc,
				StartLine: start.Line,
				EndLine:   end.Line,
			}
			if _, err := db.UpsertDefinition(def); err != nil {
				return err
			}

		case *ast.ValueSpec:
			kind := "var"
			if gd.Tok == token.CONST {
				kind = "const"
			}
			for _, name := range s.Names {
				if name.Name == "_" {
					continue
				}
				body := renderNode(fset, gd)
				doc := gd.Doc.Text()
				if doc == "" {
					doc = s.Doc.Text()
				}
				def := &store.Definition{
					ModuleID:  mod.ID,
					Name:      name.Name,
					Kind:      kind,
					Exported:  name.IsExported(),
					Signature: fmt.Sprintf("%s %s", kind, name.Name),
					Body:      body,
					Doc:       doc,
				}
				if _, err := db.UpsertDefinition(def); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
