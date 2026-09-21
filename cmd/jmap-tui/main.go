// Command jmap-tui is a JMAP-first terminal email client. The interactive
// TUI lands in M1; M0 ships the smoke subcommand that proves the connection
// path: session discovery, Basic auth, and the mailbox tree.
package main

import (
	"fmt"
	"os"
)

// version is overridden at build time via -ldflags; goreleaser sets it for
// release artifacts (NFR-6).
var version = "0.0.0-dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	switch args[0] {
	case "smoke":
		return runSmoke(args[1:])
	case "--version", "version":
		fmt.Printf("jmap-tui %s\n", version)
		return nil
	case "--help", "-h", "help":
		usage()
		return nil
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		usage()
		os.Exit(2)
		return nil
	}
}

func usage() {
	fmt.Print(`jmap-tui — a JMAP-first terminal email client

Usage:
  jmap-tui smoke [flags]   connect to a JMAP server and dump session + mailboxes (M0)
  jmap-tui version         print version

The interactive TUI arrives in M1.
`)
}
