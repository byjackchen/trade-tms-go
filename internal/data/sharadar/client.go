package sharadar

// client.go is the Go port of the Python SharadarClient
// (src/adapters/sharadar/client.py; spec docs/spec/data-sharadar.md §3):
// a thin Nasdaq Data Link "datatables" client with
//
//   - cursor pagination (the SDK's paginate=True) with streaming row
//     delivery — rows are handed to the caller page by page, never
//     accumulated for the whole logical call;
//   - the [MUST-MATCH] retry policy: 4 attempts total on HTTP 429/5xx with
//     exponential backoff (2/4/8 s waits between attempts; the original's
//     final, pointless 16 s sleep before giving up is dropped per the spec
//     §3.1 note), terminal error message shaped "... failed after 4
//     retries: ...";
//   - [IMPROVE] deviations sanctioned by spec §3.1: classification on the
//     real HTTP status code instead of substring scanning, Retry-After
//     honored (never weaker than the backoff), context-aware backoff sleeps
//     so shutdown cancels in-flight waits;
//   - fetch/cache counters and state_summary() parity (§3.1);
//   - export_table bulk download parity (§3.2).
//
// Secrets: the API key travels as the api_key query parameter (SDK parity)
// and is never logged; error messages carry dataset + status + a response
// body snippet, never the request URL.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

const (
	// DefaultBaseURL is the Nasdaq Data Link API root.
	DefaultBaseURL = "https://data.nasdaq.com/api/v3"

	// maxAttempts matches the Python _MAX_ATTEMPTS (spec §3.1).
	maxAttempts = 4

	// perPage is the page size requested from the datatables API; 10k is
	// the server-side maximum and what the Python SDK uses internally.
	perPage = 10_000

	// rowCapWarnThreshold mirrors the Python SDK's ~1M-row warning for one
	// logical call (spec Q8): the native cursor loop has no hard cap, so
	// the condition is surfaced as a warning instead of truncation.
	rowCapWarnThreshold = 1_000_000

	// apiKeyHint points the operator at where to obtain a key, matching the
	// Python client's construction-time hint (spec §1).
	apiKeyHint = "https://data.nasdaq.com/account/profile"
)

// ErrMissingAPIKey is returned by NewClient when no API key is configured.
// Fail-loud at construction, not first call (spec §1 [MUST-MATCH]).
var ErrMissingAPIKey = errors.New(
	"sharadar: Nasdaq Data Link API key is not set — set TMS_NASDAQ_DATA_LINK_API_KEY " +
		"(or NASDAQ_DATA_LINK_API_KEY); get one at " + apiKeyHint)

// Filter is one REST query filter (e.g. {"date.gte", "2024-01-02"}).
// Order is preserved on the wire.
type Filter struct {
	Key   string
	Value string
}

// DateRangeFilters builds the SEP/SFP date-window filter pair
// (`date={"gte": ..., "lte": ...}` in the Python call shape, spec §3.1).
func DateRangeFilters(gte, lte string) []Filter {
	return []Filter{{Key: "date.gte", Value: gte}, {Key: "date.lte", Value: lte}}
}

// TickersFilter builds the comma-joined ticker list filter (spec §3.1).
func TickersFilter(tickers []string) Filter {
	return Filter{Key: "ticker", Value: strings.Join(tickers, ",")}
}

// LastUpdatedGTEFilter builds the incremental-refresh filter sanctioned by
// spec §6.6 [IMPROVE] for update_sf1/update_events.
func LastUpdatedGTEFilter(date string) Filter {
	return Filter{Key: "lastupdated.gte", Value: date}
}

// RowFunc receives one decoded API row. Returning an error aborts the call.
type RowFunc func(Row) error

// TableFetcher is the client surface the sync layer depends on; tests
// substitute a fake.
type TableFetcher interface {
	GetTable(ctx context.Context, dataset string, filters []Filter, fn RowFunc) (int64, error)
}

// httpStatusError carries the HTTP status for retry classification.
type httpStatusError struct {
	dataset string
	status  int
	snippet string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("nasdaq data link %s: HTTP %d: %s", e.dataset, e.status, e.snippet)
}

// Client is the Nasdaq Data Link datatables client.
type Client struct {
	apiKey             string
	baseURL            string
	httpc              *http.Client
	log                zerolog.Logger
	backoffUnit        time.Duration // wait before retry n = 2^n * backoffUnit
	exportPollInterval time.Duration

	mu             sync.Mutex
	fetchCount     int64
	cacheHitCount  int64
	cacheMissCount int64
	lastFetchNS    int64
}

// ClientOption customizes a Client.
type ClientOption func(*Client)

// WithBaseURL overrides the API root (tests point it at httptest servers).
func WithBaseURL(u string) ClientOption {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(h *http.Client) ClientOption { return func(c *Client) { c.httpc = h } }

// WithLogger attaches a structured logger.
func WithLogger(l zerolog.Logger) ClientOption { return func(c *Client) { c.log = l } }

// WithBackoffUnit scales the retry schedule (production default 1s gives
// the spec'd 2/4/8 s waits; tests shrink it).
func WithBackoffUnit(d time.Duration) ClientOption { return func(c *Client) { c.backoffUnit = d } }

// WithExportPollInterval sets the export_table readiness poll cadence.
func WithExportPollInterval(d time.Duration) ClientOption {
	return func(c *Client) { c.exportPollInterval = d }
}

// NewClient builds a Client. An empty API key fails loud here, mirroring
// the Python constructor (spec §1 [MUST-MATCH]); an explicitly passed key
// always wins over config (the caller resolves config precedence).
func NewClient(apiKey string, opts ...ClientOption) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, ErrMissingAPIKey
	}
	c := &Client{
		apiKey:             apiKey,
		baseURL:            DefaultBaseURL,
		httpc:              &http.Client{Timeout: 5 * time.Minute},
		log:                zerolog.Nop(),
		backoffUnit:        time.Second,
		exportPollInterval: 30 * time.Second,
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// get_table
// ---------------------------------------------------------------------------

// GetTable fetches a Sharadar datatable, following cursor pages until
// exhausted, and streams each row to fn. It returns the total row count.
//
// Counters parity (spec §3.1 [MUST-MATCH]): one successful logical call
// increments fetch_count and cache_miss_count and stamps last_fetch_ts;
// failed calls increment nothing.
func (c *Client) GetTable(ctx context.Context, dataset string, filters []Filter, fn RowFunc) (int64, error) {
	var (
		rows   int64
		cursor string
		pages  int
	)
	for {
		pg, err := c.fetchPage(ctx, dataset, filters, cursor)
		if err != nil {
			return rows, err
		}
		pages++
		idx := pg.colIndex()
		for i, vals := range pg.rows {
			if len(vals) != len(pg.columns) {
				return rows, fmt.Errorf("sharadar: get_table %s: page %d row %d has %d cells, want %d columns",
					dataset, pages, i, len(vals), len(pg.columns))
			}
			rows++
			if err := fn(Row{cols: idx, vals: vals}); err != nil {
				return rows, err
			}
		}
		cursor = pg.nextCursor
		if cursor == "" {
			break
		}
	}

	if rows > rowCapWarnThreshold {
		// Spec Q8: the Python SDK silently stops near 1M rows; the native
		// loop fetches everything but flags calls the original could not
		// have completed.
		c.log.Warn().Str("dataset", dataset).Int64("rows", rows).
			Msg("get_table exceeded the Python SDK's ~1M row cap for a single call")
	}

	c.mu.Lock()
	c.fetchCount++
	c.cacheMissCount++
	c.lastFetchNS = time.Now().UnixNano()
	c.mu.Unlock()

	c.log.Debug().Str("dataset", dataset).Int64("rows", rows).Int("pages", pages).
		Msg("get_table complete")
	return rows, nil
}

// fetchPage requests one cursor page with the retry policy.
func (c *Client) fetchPage(ctx context.Context, dataset string, filters []Filter, cursor string) (*page, error) {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		pg, retryAfter, err := c.requestPage(ctx, dataset, filters, cursor)
		if err == nil {
			return pg, nil
		}
		lastErr = err

		var he *httpStatusError
		retryable := errors.As(err, &he) && (he.status == http.StatusTooManyRequests || he.status >= 500)
		if !retryable {
			return nil, err
		}
		if attempt == maxAttempts {
			break
		}
		wait := time.Duration(1<<uint(attempt)) * c.backoffUnit // 2, 4, 8 × unit
		if retryAfter > wait {
			wait = retryAfter // Retry-After honored, never weaker (spec §3.1 [IMPROVE])
		}
		c.log.Warn().Str("dataset", dataset).Int("attempt", attempt).Int("status", he.status).
			Dur("wait", wait).Msg("get_table attempt failed; retrying")
		if err := sleepCtx(ctx, wait); err != nil {
			return nil, fmt.Errorf("sharadar: get_table %s canceled during backoff: %w", dataset, err)
		}
	}
	return nil, fmt.Errorf("sharadar: get_table %s failed after %d retries: %w", dataset, maxAttempts, lastErr)
}

// requestPage performs one HTTP round trip. On HTTP errors it returns an
// *httpStatusError plus any parsed Retry-After duration.
func (c *Client) requestPage(ctx context.Context, dataset string, filters []Filter, cursor string) (*page, time.Duration, error) {
	q := url.Values{}
	for _, f := range filters {
		q.Add(f.Key, f.Value)
	}
	q.Set("qopts.per_page", strconv.Itoa(perPage))
	if cursor != "" {
		q.Set("qopts.cursor_id", cursor)
	}
	q.Set("api_key", c.apiKey)

	reqURL := c.baseURL + "/datatables/" + dataset + ".json?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("sharadar: building request for %s: %w", dataset, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpc.Do(req)
	if err != nil {
		// Transport errors: strip the URL (it embeds api_key) before
		// surfacing. url.Error wraps the cause; keep only the cause text.
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, 0, fmt.Errorf("sharadar: get_table %s: %s %w", dataset, ue.Op, ue.Err)
		}
		return nil, 0, fmt.Errorf("sharadar: get_table %s: %w", dataset, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, parseRetryAfter(resp.Header.Get("Retry-After")), &httpStatusError{
			dataset: dataset,
			status:  resp.StatusCode,
			snippet: strings.TrimSpace(string(snippet)),
		}
	}

	pg, err := decodePage(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("sharadar: get_table %s: %w", dataset, err)
	}
	return pg, 0, nil
}

// parseRetryAfter handles both delta-seconds and HTTP-date forms.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// sleepCtx is an interruptible sleep (spec §3.1 [IMPROVE]: time.sleep was
// uninterruptible in the original).
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---------------------------------------------------------------------------
// Cache instrumentation + state summary (spec §3.1 [MUST-MATCH])
// ---------------------------------------------------------------------------

// RecordCacheHit records that a local cache served a response without an
// API call.
func (c *Client) RecordCacheHit() {
	c.mu.Lock()
	c.cacheHitCount++
	c.mu.Unlock()
}

// StateSummary returns the JSON-serializable counter snapshot with the
// exact key set of the Python client: source, fetch_count, cache_hit_count,
// cache_miss_count, last_fetch_ts (ISO 8601 UTC or nil), last_fetch_ts_ns,
// quota_used_today (always nil — the API does not expose quota).
func (c *Client) StateSummary() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()

	var lastFetchISO any
	if c.lastFetchNS > 0 {
		lastFetchISO = time.Unix(0, c.lastFetchNS).UTC().Format("2006-01-02T15:04:05.000000+00:00")
	}
	return map[string]any{
		"source":           "sharadar",
		"fetch_count":      c.fetchCount,
		"cache_hit_count":  c.cacheHitCount,
		"cache_miss_count": c.cacheMissCount,
		"last_fetch_ts":    lastFetchISO,
		"last_fetch_ts_ns": c.lastFetchNS,
		"quota_used_today": nil,
	}
}

// ---------------------------------------------------------------------------
// export_table (spec §3.2 [MUST-MATCH])
// ---------------------------------------------------------------------------

// exportStatus mirrors the datatable_bulk_download response envelope.
type exportStatus struct {
	DatatableBulkDownload struct {
		File struct {
			Link   string `json:"link"`
			Status string `json:"status"`
		} `json:"file"`
	} `json:"datatable_bulk_download"`
}

// ExportTable triggers Sharadar's async bulk export, polls until the file
// is ready, downloads the zip to targetDir and returns its path. Filename:
// <targetDir>/<dataset with "/" -> "_">.zip (spec §3.2). Currently unused
// by the sync paths (bootstrap goes through paginated GetTable) but kept
// for completeness, like the original.
func (c *Client) ExportTable(ctx context.Context, dataset, targetDir string, filters []Filter) (string, error) {
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return "", fmt.Errorf("sharadar: export_table %s: creating %s: %w", dataset, targetDir, err)
	}
	outPath := filepath.Join(targetDir, strings.ReplaceAll(dataset, "/", "_")+".zip")

	for {
		st, err := c.requestExportStatus(ctx, dataset, filters)
		if err != nil {
			return "", err
		}
		file := st.DatatableBulkDownload.File
		if file.Status == "fresh" && file.Link != "" {
			if err := c.download(ctx, dataset, file.Link, outPath); err != nil {
				return "", err
			}
			return outPath, nil
		}
		c.log.Info().Str("dataset", dataset).Str("status", file.Status).
			Dur("poll_interval", c.exportPollInterval).Msg("export not ready; polling")
		if err := sleepCtx(ctx, c.exportPollInterval); err != nil {
			return "", fmt.Errorf("sharadar: export_table %s canceled while polling: %w", dataset, err)
		}
	}
}

// requestExportStatus performs one qopts.export=true poll with the same
// retry policy as get_table.
func (c *Client) requestExportStatus(ctx context.Context, dataset string, filters []Filter) (*exportStatus, error) {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		st, retryAfter, err := c.tryExportStatus(ctx, dataset, filters)
		if err == nil {
			return st, nil
		}
		lastErr = err
		var he *httpStatusError
		retryable := errors.As(err, &he) && (he.status == http.StatusTooManyRequests || he.status >= 500)
		if !retryable {
			return nil, err
		}
		if attempt == maxAttempts {
			break
		}
		wait := time.Duration(1<<uint(attempt)) * c.backoffUnit
		if retryAfter > wait {
			wait = retryAfter
		}
		c.log.Warn().Str("dataset", dataset).Int("attempt", attempt).Int("status", he.status).
			Dur("wait", wait).Msg("export_table attempt failed; retrying")
		if err := sleepCtx(ctx, wait); err != nil {
			return nil, fmt.Errorf("sharadar: export_table %s canceled during backoff: %w", dataset, err)
		}
	}
	return nil, fmt.Errorf("sharadar: export_table %s failed after %d retries: %w", dataset, maxAttempts, lastErr)
}

func (c *Client) tryExportStatus(ctx context.Context, dataset string, filters []Filter) (*exportStatus, time.Duration, error) {
	q := url.Values{}
	for _, f := range filters {
		q.Add(f.Key, f.Value)
	}
	q.Set("qopts.export", "true")
	q.Set("api_key", c.apiKey)

	reqURL := c.baseURL + "/datatables/" + dataset + ".json?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("sharadar: building export request for %s: %w", dataset, err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, 0, fmt.Errorf("sharadar: export_table %s: %s %w", dataset, ue.Op, ue.Err)
		}
		return nil, 0, fmt.Errorf("sharadar: export_table %s: %w", dataset, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, parseRetryAfter(resp.Header.Get("Retry-After")), &httpStatusError{
			dataset: dataset,
			status:  resp.StatusCode,
			snippet: strings.TrimSpace(string(snippet)),
		}
	}

	var st exportStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, 0, fmt.Errorf("sharadar: export_table %s: decoding status: %w", dataset, err)
	}
	return &st, 0, nil
}

// download streams the export zip to outPath via a temp file + atomic
// rename, so a partial download never masquerades as a complete export.
func (c *Client) download(ctx context.Context, dataset, link, outPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return fmt.Errorf("sharadar: export_table %s: building download request: %w", dataset, err)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("sharadar: export_table %s: downloading: %w", dataset, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sharadar: export_table %s: download returned HTTP %d", dataset, resp.StatusCode)
	}

	tmp := outPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("sharadar: export_table %s: creating %s: %w", dataset, tmp, err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("sharadar: export_table %s: writing %s: %w", dataset, tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("sharadar: export_table %s: closing %s: %w", dataset, tmp, err)
	}
	if err := os.Rename(tmp, outPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("sharadar: export_table %s: renaming to %s: %w", dataset, outPath, err)
	}
	return nil
}
