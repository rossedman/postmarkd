// Command generated prints the module-qualified name of every generated Go
// file, one per line, in the form coverage profiles use. The coverage gate
// filters these out so only hand-written code is measured.
//
//	go run ./tools/generated ./...
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/rossedman/postmarkd/tools/internal/gofiles"
)

// main lists the generated files in the packages named on the command line.
func main() {
	os.Exit(run(".", os.Args[1:], os.Stdout, os.Stderr))
}

// run prints the generated files matching patterns under root and returns
// the process exit code.
func run(root string, patterns []string, stdout, stderr io.Writer) int {
	err := gofiles.Walk(root, patterns, func(f gofiles.File) error {
		if f.Generated {
			_, err := fmt.Fprintln(stdout, f.ImportPath)
			return err
		}
		return nil
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "generated:", err)
		return 2
	}
	return 0
}
