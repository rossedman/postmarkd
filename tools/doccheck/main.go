// Command doccheck fails if any function or method lacks a doc comment, or
// any package lacks a package comment. Test files and generated files are
// skipped.
//
//	go run ./tools/doccheck ./...
package main

import (
	"fmt"
	"go/ast"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/rossedman/postmarkd/tools/internal/gofiles"
)

// main runs the check over the packages named on the command line.
func main() {
	os.Exit(run(".", os.Args[1:], os.Stdout, os.Stderr))
}

// run checks the packages matching patterns under root, printing one line
// per problem to stdout. It returns the process exit code.
func run(root string, patterns []string, stdout, stderr io.Writer) int {
	var problems []string
	documented := map[string]bool{} // package directory -> has a package comment
	err := gofiles.Walk(root, patterns, func(f gofiles.File) error {
		if f.Generated {
			return nil
		}
		dir := filepath.Dir(f.Path)
		documented[dir] = documented[dir] || f.AST.Doc != nil
		for _, d := range f.AST.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Doc != nil {
				continue
			}
			pos := f.Fset.Position(fd.Pos())
			problems = append(problems, fmt.Sprintf("%s:%d: %s has no doc comment", f.Path, pos.Line, funcName(fd)))
		}
		return nil
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "doccheck:", err)
		return 2
	}
	var dirs []string
	for dir, ok := range documented {
		if !ok {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		problems = append(problems, fmt.Sprintf("%s: package has no package comment", dir))
	}
	for _, p := range problems {
		_, _ = fmt.Fprintln(stdout, p)
	}
	if len(problems) > 0 {
		_, _ = fmt.Fprintf(stderr, "doccheck: %d problem(s)\n", len(problems))
		return 1
	}
	return 0
}

// funcName names a function as Name or (Recv).Name for methods.
func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if g, ok := t.(*ast.IndexExpr); ok {
		t = g.X
	}
	if g, ok := t.(*ast.IndexListExpr); ok {
		t = g.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}
