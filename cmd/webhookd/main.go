// Package main is the entry point for the webhookd webhook receiver service.
//
// This file is a Phase 0 placeholder. Full application wiring — config load,
// observability initialization, HTTP listeners, and graceful shutdown — lands
// in Phase 5 of IMPL-0001.
package main

import (
	"fmt"
	"os"
	"runtime"
)

// Build-time provenance, injected via
// -ldflags "-X main.version=... -X main.commit=...".
// See the build target in the Makefile.
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	fmt.Fprintf(
		os.Stderr,
		"webhookd %s (commit %s, %s) — Phase 0 placeholder\n",
		version, commit, runtime.Version(),
	)
}
