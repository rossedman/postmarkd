// Command postmarkd relays mail through Postmark. Run without a command (or
// with "serve") it is the daemon; other commands are small clients for it.
// Invoked as "sendmail" it behaves like sendmail(8).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/rossedman/postmarkd/internal/domaincheck"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

const usage = `postmarkd relays mail through Postmark.

Usage:
  postmarkd [serve] [flags]        run the daemon (default)
  postmarkd send [flags]           send a message from the command line
  postmarkd sendmail [flags] [to…] sendmail(8)-compatible: read a message on stdin
  postmarkd check-domain <domain>  check DKIM, Return-Path and DMARC DNS records
  postmarkd healthcheck            exit 0 if the daemon is healthy
  postmarkd version                print the version

Run "postmarkd <command> -h" for a command's flags.
Symlink the binary as /usr/sbin/sendmail to make every program that sends
mail with sendmail use postmarkd.

Docs: https://github.com/rossedman/postmarkd
`

// cli holds what the commands take from the process, so tests can run them
// with their own input, output and context.
type cli struct {
	ctx            context.Context
	stdin          io.Reader
	stdout, stderr io.Writer
	// stdinPiped reports whether stdin is a pipe or file rather than a
	// terminal.
	stdinPiped func() bool
	// checkDomain runs the DNS checks for check-domain.
	checkDomain func(context.Context, domaincheck.Options) []domaincheck.Check
}

// main runs the command named in os.Args and exits with its status.
func main() {
	c := &cli{
		ctx:         context.Background(),
		stdin:       os.Stdin,
		stdout:      os.Stdout,
		stderr:      os.Stderr,
		stdinPiped:  stdinPiped,
		checkDomain: domaincheck.Run,
	}
	os.Exit(c.run(os.Args))
}

// run dispatches argv (including the program name) to a command and returns
// the exit status.
func (c *cli) run(argv []string) int {
	if filepath.Base(argv[0]) == "sendmail" {
		return c.sendmail(argv[1:])
	}
	if len(argv) < 2 || argv[1] == "" || argv[1][0] == '-' {
		return c.serve(argv[1:])
	}
	cmd, args := argv[1], argv[2:]
	switch cmd {
	case "serve":
		return c.serve(args)
	case "send":
		return c.send(args)
	case "sendmail":
		return c.sendmail(args)
	case "check-domain":
		return c.checkDomainCmd(args)
	case "healthcheck":
		return c.healthcheck(args)
	case "version":
		c.printf("postmarkd %s\n", version)
	case "help":
		c.printf("%s", usage)
	default:
		c.errorf("postmarkd: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
	return 0
}

// flags returns a flag set that reports errors to stderr instead of exiting.
func (c *cli) flags(name string) *flag.FlagSet {
	fl := flag.NewFlagSet(name, flag.ContinueOnError)
	fl.SetOutput(c.stderr)
	return fl
}

// parse parses args into fl. It returns -1 on success, or the exit status
// when parsing stopped: 0 after -h, 2 for a bad flag, as flag.ExitOnError
// would.
func parse(fl *flag.FlagSet, args []string) int {
	err := fl.Parse(args)
	switch {
	case err == nil:
		return -1
	case errors.Is(err, flag.ErrHelp):
		return 0
	default:
		return 2
	}
}

// printf writes to stdout. Write errors are ignored, as fmt.Printf callers
// usually do: there is nowhere left to report them.
func (c *cli) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(c.stdout, format, args...)
}

// errorf writes to stderr, ignoring write errors like printf.
func (c *cli) errorf(format string, args ...any) {
	_, _ = fmt.Fprintf(c.stderr, format, args...)
}

// stdinPiped reports whether the process's stdin is not a terminal.
func stdinPiped() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice == 0
}
