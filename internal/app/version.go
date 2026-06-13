package app

import (
	"fmt"
	"runtime"
)

// Build metadata, injected at link time via:
//
//	-ldflags "-X github.com/byjackchen/trade-tms-go/internal/app.Version=... \
//	          -X github.com/byjackchen/trade-tms-go/internal/app.Commit=... \
//	          -X github.com/byjackchen/trade-tms-go/internal/app.BuildDate=..."
var (
	// Version is the semantic version or git describe output.
	Version = "dev"
	// Commit is the short git commit hash of the build.
	Commit = "none"
	// BuildDate is the RFC3339 UTC timestamp of the build.
	BuildDate = "unknown"
)

// VersionString renders a single-line, human-readable version banner.
func VersionString() string {
	return fmt.Sprintf("tms %s (commit %s, built %s, %s, %s/%s)",
		Version, Commit, BuildDate, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
