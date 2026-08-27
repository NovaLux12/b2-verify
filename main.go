// Command b2-verify is a backup-freshness watchdog. It watches the output of
// backup pipelines (the newest object in a B2 bucket, or the newest file in a
// directory) and alerts when a leg goes stale, so silent backup rot is caught
// within hours rather than discovered weeks later during an audit.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "v0.1.0-dev"

const usageText = `b2-verify - backup freshness watchdog

Usage:
  b2-verify [flags] <command>

Commands:
  run     Run the continuous monitor: check every interval, serve /metrics.
  check   One-shot check. Exit code: 0 = all legs fresh, 1 = a leg is stale,
          2 = a leg failed to check or the config is invalid.

Flags:
  -config FILE  Path to the JSON configuration file (default "config.json").
  -dry-run      Evaluate legs without sending alerts (log-only).
  -version      Print the version and exit.
  -h, -help     Show this help text.

The configuration format and examples are documented in the README.
`

func main() {
	fs := flag.NewFlagSet("b2-verify", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath, "path to the JSON configuration file")
	dryRun := fs.Bool("dry-run", false, "evaluate without sending alerts (log-only)")
	showVersion := fs.Bool("version", false, "print version and exit")
	fs.Usage = func() { fmt.Fprint(fs.Output(), usageText) }
	fs.Parse(os.Args[1:])
	args := fs.Args()
	if len(args) == 0 {
		fs.Usage()
		os.Exit(2)
	}
	cmd := args[0]
	if len(args) > 1 {
		// The command came first with flags after it, e.g. "check -config x".
		// The flag package stops at the first positional argument, so reparse
		// the tail to pick up trailing flags.
		fs.Parse(args[1:])
	}

	if *showVersion {
		fmt.Printf("b2-verify %s\n", version)
		return
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		slog.Error("cannot load configuration", "path", *configPath, "error", err)
		os.Exit(2)
	}

	switch cmd {
	case "run":
		runMonitor(cfg, *dryRun)
	case "check":
		os.Exit(runCheck(cfg, *dryRun))
	default:
		fmt.Fprintf(os.Stderr, "b2-verify: unknown command %q\n", cmd)
		fs.Usage()
		os.Exit(2)
	}
}
