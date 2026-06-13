// Package adapters contains every integration with the outside world:
// Sharadar/Nasdaq Data Link HTTP client, moomoo OpenD gateway client,
// Redis streams publisher/consumer for live telemetry, and any future
// broker or data vendor. Go counterpart of the reference's src/adapters/.
//
// Rules:
//   - Adapters depend on internal/config for credentials — never read
//     os.Environ directly.
//   - All network calls take a context, time out, and surface typed errors;
//     retries/backoff live here, not in the engine.
package adapters
