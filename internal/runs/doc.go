// Package runs manages run artifacts: backtest result dumps, hyperopt
// studies, active_params selection and EOD reports — the Go counterpart of
// the reference repo's runs/ directory conventions and src/runs/ helpers.
// It defines the on-disk/DB layout, metadata (git rev, params digest,
// universe snapshot) and lookup APIs the HTTP API serves to the UI.
//
// Rules:
//   - Artifact layout stays stable across releases so existing dumps remain
//     readable.
//   - Every run records enough metadata to be reproduced exactly.
package runs
