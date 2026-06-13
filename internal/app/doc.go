// Package app provides process-level plumbing shared by every tms
// subcommand: structured logging (zerolog) configured from config,
// signal-aware root contexts and graceful-shutdown helpers, and build/
// version information injected at link time.
//
// Rules:
//   - No business logic; only process lifecycle and observability glue.
//   - Normal control flow never panics; errors propagate to main, which
//     logs once and exits non-zero.
package app
