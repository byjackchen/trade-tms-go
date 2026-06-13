package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
)

// loadDotenv mirrors python-dotenv's find_dotenv(usecwd=True) +
// load_dotenv(override=False): walk up from the working directory looking
// for a `.env` file, parse it with godotenv, and set only variables that
// are not already present in the process environment (godotenv.Load never
// overrides existing vars). Returns the path loaded ("" if no .env was
// found). Idempotent: re-running never overrides anything.
func loadDotenv() (string, error) {
	path, err := findDotenv()
	if err != nil || path == "" {
		return "", err
	}
	if err := godotenv.Load(path); err != nil {
		return "", fmt.Errorf("malformed .env file %s: %w", path, err)
	}
	return path, nil
}

// findDotenv walks from the current working directory up to the filesystem
// root and returns the first `.env` regular file found, or "".
func findDotenv() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolving working directory: %w", err)
	}
	for {
		candidate := filepath.Join(dir, ".env")
		info, err := os.Stat(candidate)
		switch {
		case err == nil && info.Mode().IsRegular():
			return candidate, nil
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return "", fmt.Errorf("checking %s: %w", candidate, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}
