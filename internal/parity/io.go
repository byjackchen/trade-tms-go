package parity

// io.go holds the parity package's tiny atomic file writer, used for the
// parity-specific artifacts (fills.json, equity.json) that the standard runs
// dumper does not emit.

import (
	"fmt"
	"os"
)

// atomicWriteJSON writes body to path atomically (tmp file + rename), matching
// the runs dumper's durability convention.
func atomicWriteJSON(path string, body []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("parity: writing %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("parity: renaming %s: %w", path, err)
	}
	return nil
}
