//go:build parity

// parity_gate_test.go is the PERMANENT golden-parity integration gate. It runs
// only under the `parity` build tag (it shells out to the reference Nautilus
// harness, which needs the read-only trade-multi-strategies venv + Sharadar
// cache), so the default `go test ./...` stays hermetic.
//
// Run it via the Makefile, which wires MS_REPO / MS_PY:
//
//	make parity                      # full gate (nautilus + go + compare)
//	go test -tags parity ./internal/parity/   # this test (drives `make parity`)
//
// The test invokes `make parity` from the repo root and asserts a zero exit:
// the comparator (tmp/parity/compare_engine.py) prints a structured per-field
// report and exits non-zero on any divergence beyond tolerance (prices exact
// after fixed-point, pnl/equity within a cent, counts + ordering exact).

package parity

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestGoldenParityGate(t *testing.T) {
	repoRoot := repoRootDir(t)

	// Allow overriding the reference repo / interpreter from the environment so
	// CI can point at a different checkout; the Makefile defaults otherwise.
	args := []string{"parity"}
	if v := os.Getenv("MS_REPO"); v != "" {
		args = append(args, "MS_REPO="+v)
	}
	if v := os.Getenv("MS_PY"); v != "" {
		args = append(args, "MS_PY="+v)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "make", args...)
	cmd.Dir = repoRoot
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	t.Logf("make parity output:\n%s", out)
	if err != nil {
		t.Fatalf("golden parity gate failed: %v", err)
	}
}

// repoRootDir walks up from the test's working directory to the module root
// (the directory containing go.mod / Makefile).
func repoRootDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repo root (go.mod) from test dir")
		}
		dir = parent
	}
}
