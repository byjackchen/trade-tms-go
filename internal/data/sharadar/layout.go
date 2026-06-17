package sharadar

// layout.go is the cache layout (spec §4): path map of the on-disk parquet
// cache and cache-root resolution.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Dataset names as the cache spells them (meta keys, dir names, spec §5).
const (
	DatasetTickers = "TICKERS"
	DatasetSEP     = "SEP"
	DatasetSFP     = "SFP"
	DatasetSF1     = "SF1"
	DatasetEvents  = "EVENTS"
)

// DatasetOrder is the canonical import order TICKERS -> SEP -> SFP -> SF1 ->
// EVENTS (spec §9).
var DatasetOrder = []string{DatasetTickers, DatasetSEP, DatasetSFP, DatasetSF1, DatasetEvents}

// ErrCacheDirNotFound reports that no Sharadar cache root could be resolved.
var ErrCacheDirNotFound = errors.New("sharadar: cache directory not found")

// ResolveCacheDir resolves the cache root with this precedence (spec §1):
//
//  1. explicit (CLI flag) if non-empty after trimming, ~ expanded;
//  2. configured (TMS_SHARADAR_CACHE_DIR) if non-empty after trimming;
//  3. walk up from the working directory until a directory containing a
//     repo marker (go.mod or pyproject.toml) is found; cache root =
//     <that dir>/cache/sharadar.
//
// There is no home-dir fallback; if nothing matches, the error tells the
// operator to set TMS_SHARADAR_CACHE_DIR.
// The resolved path must exist and be a directory.
func ResolveCacheDir(explicit, configured string) (string, error) {
	for _, cand := range []string{strings.TrimSpace(explicit), strings.TrimSpace(configured)} {
		if cand == "" {
			continue
		}
		dir, err := expandHome(cand)
		if err != nil {
			return "", err
		}
		return requireDir(dir)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("sharadar: getting working directory: %w", err)
	}
	for dir := cwd; ; {
		for _, marker := range []string{"go.mod", "pyproject.toml"} {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return requireDir(filepath.Join(dir, "cache", "sharadar"))
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("%w: no repo marker (go.mod/pyproject.toml) above %s — set TMS_SHARADAR_CACHE_DIR or pass --cache-dir", ErrCacheDirNotFound, cwd)
		}
		dir = parent
	}
}

func expandHome(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("sharadar: expanding ~ in %q: %w", p, err)
		}
		return filepath.Join(home, strings.TrimPrefix(p[1:], "/")), nil
	}
	return p, nil
}

func requireDir(dir string) (string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("%w: %s (set TMS_SHARADAR_CACHE_DIR or pass --cache-dir)", ErrCacheDirNotFound, dir)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: %s is not a directory", ErrCacheDirNotFound, dir)
	}
	return dir, nil
}

// tickersPath is the universe master file (spec §4: TICKERS.parquet).
func tickersPath(root string) string { return filepath.Join(root, "TICKERS.parquet") }

// yearPartition is one SEP/SFP year file.
type yearPartition struct {
	Year int
	Path string
}

// yearPartitions lists <root>/<dataset>/year=YYYY/part-*.parquet sorted by
// (year, filename). The writer emits exactly part-0.parquet, but stats() in
// the reference globs part-*.parquet, so multi-part is tolerated (spec §4).
func yearPartitions(root, dataset string) ([]yearPartition, error) {
	pattern := filepath.Join(root, dataset, "year=*", "part-*.parquet")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("sharadar: globbing %s: %w", pattern, err)
	}
	parts := make([]yearPartition, 0, len(matches))
	for _, m := range matches {
		dir := filepath.Base(filepath.Dir(m))
		yearStr, ok := strings.CutPrefix(dir, "year=")
		if !ok {
			continue
		}
		year, err := strconv.Atoi(yearStr)
		if err != nil {
			continue // not a year partition; ignore foreign dirs
		}
		parts = append(parts, yearPartition{Year: year, Path: m})
	}
	sort.Slice(parts, func(i, j int) bool {
		if parts[i].Year != parts[j].Year {
			return parts[i].Year < parts[j].Year
		}
		return parts[i].Path < parts[j].Path
	})
	return parts, nil
}

// tickerFile is one SF1/EVENTS per-ticker file.
type tickerFile struct {
	Ticker string
	Path   string
}

// perTickerFiles lists <root>/<dataset>/ticker=<T>.parquet sorted by ticker.
// The ticker is recovered from the filename as the layout embeds it (raw
// symbol, may contain '.' or '-', spec §4).
func perTickerFiles(root, dataset string) ([]tickerFile, error) {
	pattern := filepath.Join(root, dataset, "ticker=*.parquet")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("sharadar: globbing %s: %w", pattern, err)
	}
	files := make([]tickerFile, 0, len(matches))
	for _, m := range matches {
		base := filepath.Base(m)
		ticker := strings.TrimSuffix(strings.TrimPrefix(base, "ticker="), ".parquet")
		if ticker == "" {
			continue
		}
		files = append(files, tickerFile{Ticker: ticker, Path: m})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Ticker < files[j].Ticker })
	return files, nil
}
