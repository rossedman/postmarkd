// Command postmarkd relays mail through Postmark. Run without a command (or
// with "serve") it is the daemon; other commands are small clients for it.
// Invoked as "sendmail" it behaves like sendmail(8).
package main

import (
	"fmt"
	"os"
	"path/filepath"
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

func main() {
	if name := filepath.Base(os.Args[0]); name == "sendmail" {
		os.Exit(sendmail(os.Args[1:]))
	}
	if len(os.Args) < 2 || os.Args[1] == "" || os.Args[1][0] == '-' {
		os.Exit(serve(os.Args[1:]))
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "serve":
		os.Exit(serve(args))
	case "send":
		os.Exit(send(args))
	case "sendmail":
		os.Exit(sendmail(args))
	case "check-domain":
		os.Exit(checkDomain(args))
	case "healthcheck":
		os.Exit(healthcheck(args))
	case "version":
		fmt.Println("postmarkd", version)
	case "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "postmarkd: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
}
