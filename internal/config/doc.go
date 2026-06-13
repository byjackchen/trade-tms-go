// Package config is the single source of truth for runtime configuration,
// mirroring the Python reference's src/config.py philosophy: load .env
// (non-overriding, found by walking up from the working directory), expose
// a typed read-only Config snapshot, and fail loud and fast with
// MissingConfig errors that carry setup hints — catching a missing value at
// startup beats catching it mid-trading-day.
//
// Rules:
//   - Every other package reads configuration through this package, never
//     os.Getenv directly, so tests can inject a Config and missing values
//     produce consistent errors.
//   - Load returns a fresh snapshot each call (no hidden module singleton
//     mutation).
package config
