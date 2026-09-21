// Command jmap-tui is a JMAP-first terminal email client. The default
// command runs the interactive reader (M1); `smoke` proves the connection
// path (M0).
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
		return runTUI(nil)
	}
	switch args[0] {
	case "smoke":
		return runSmoke(args[1:])
	case "tui":
		return runTUI(args[1:])
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
  jmap-tui                 run the interactive reader (M1)
  jmap-tui smoke [flags]   connect and dump session + mailboxes (M0)
  jmap-tui version         print version

TUI flags:
  --config PATH       config file (default: $XDG_CONFIG_HOME/jmap-tui/config.toml)
  --account ID        account from config (default: default_account)
  --theme THEME       dark, light, or auto (default)
  --log-file PATH     write a redacted debug log (off by default)
  --log-level LEVEL   debug | info | warn | error (with --log-file)
`)
}
