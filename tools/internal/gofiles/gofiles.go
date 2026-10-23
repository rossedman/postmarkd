// Package gofiles walks the Go source files of this module for the repo
// checks in tools/. It understands the "./..." and "dir/..." package
// patterns, skips test files, testdata, hidden directories and nested
// modules, and reports whether each file is generated.
package gofiles

import (
	"bufio"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// File is one parsed, non-test Go source file.
type File struct {
	// Path is the file's path as found on disk, relative to the walk root.
	Path string
	// ImportPath is the module-qualified name used in coverage profiles,
	// e.g. github.com/owner/repo/internal/x/x.go.
	ImportPath string
	// AST is the parsed file, with comments.
	AST *ast.File
	// Fset positions AST.
	Fset *token.FileSet
	// Generated reports whether the file carries the standard
	// "Code generated ... DO NOT EDIT." header.
	Generated bool
}

// Walk calls fn for every non-test Go file matched by patterns, which are
// directories relative to root, optionally ending in "/..." to include
// subdirectories. Files are visited in lexical order.
func Walk(root string, patterns []string, fn func(File) error) error {
	module, err := modulePath(filepath.Join(root, "go.mod"))
	if err != nil {
		return err
	}
	if len(patterns) == 0 {
		patterns = []string{"."}
	}
	seen := map[string]bool{}
	var files []string
	for _, p := range patterns {
		dir, recursive := strings.CutSuffix(p, "...")
		dir = filepath.Clean(strings.TrimSuffix(dir, "/"))
		if dir == "" {
			dir = "."
		}
		found, err := goFiles(root, dir, recursive)
		if err != nil {
			return err
		}
		for _, f := range found {
			if !seen[f] {
				seen[f] = true
				files = append(files, f)
			}
		}
	}
	sort.Strings(files)

	for _, rel := range files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, parser.ParseComments)
		if err != nil {
			return err
		}
		err = fn(File{
			Path:       rel,
			ImportPath: path.Join(module, filepath.ToSlash(rel)),
			AST:        f,
			Fset:       fset,
			Generated:  ast.IsGenerated(f),
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// goFiles lists the non-test .go files under root/dir, relative to root.
func goFiles(root, dir string, recursive bool) ([]string, error) {
	var out []string
	start := filepath.Join(root, dir)
	err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == start {
				return nil
			}
			if !recursive || skipDir(p, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking %s: %w", dir, err)
	}
	return out, nil
}

// skipDir reports whether the go tool would leave the directory out of a
// "..." pattern: testdata, hidden and underscore directories, and nested
// modules.
func skipDir(p, name string) bool {
	if name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
		return true
	}
	_, err := os.Stat(filepath.Join(p, "go.mod"))
	return err == nil
}

// modulePath reads the module path from a go.mod file.
func modulePath(gomod string) (string, error) {
	f, err := os.Open(gomod)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "//")
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", errors.New(gomod + ": no module directive")
}
