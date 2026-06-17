package api

// stub_test.go provides in-memory implementations of the API's persistence
// seams (JobQueue, DataStore, UniverseReader) plus a helper that builds a
// fully wired *Server over a fixed clock and a real (offline) NYSE calendar.
// The contract tests in handlers_test.go and ws_test.go drive these stubs
// through httptest so every endpoint is exercised without a database.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/universe"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/model"
	"github.com/byjackchen/trade-tms-go/internal/runs"
)

// testToken is the bearer token every contract test authenticates with.
const testToken = "secret-test-token"

// testOrigin is the single allowlisted CORS / WebSocket origin.
const testOrigin = "http://localhost:13000"

// fixedNow is the clock the server sees in tests: a real NYSE session
// (Wednesday 2024-06-12, well inside the calendar's 2000-2030 range).
var fixedNow = time.Date(2024, time.June, 12, 15, 30, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// stubJobQueue
// ---------------------------------------------------------------------------

// stubJobQueue is an in-memory JobQueue. Each method can be overridden with a
// func field to inject errors / custom behavior; otherwise sensible defaults
// run against the jobs map.
type stubJobQueue struct {
	mu       sync.Mutex
	jobs     map[int64]*jobs.Job
	nextID   int64
	enqueued []jobs.EnqueueParams

	enqueueFn func(ctx context.Context, p jobs.EnqueueParams) (*jobs.Job, bool, error)
	listErr   error
}

func newStubJobQueue() *stubJobQueue {
	return &stubJobQueue{jobs: map[int64]*jobs.Job{}, nextID: 0}
}

func (q *stubJobQueue) add(j *jobs.Job) *jobs.Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	if j.ID == 0 {
		q.nextID++
		j.ID = q.nextID
	} else if j.ID > q.nextID {
		q.nextID = j.ID
	}
	q.jobs[j.ID] = j
	return j
}

func (q *stubJobQueue) Enqueue(ctx context.Context, p jobs.EnqueueParams) (*jobs.Job, bool, error) {
	q.mu.Lock()
	q.enqueued = append(q.enqueued, p)
	q.mu.Unlock()
	if q.enqueueFn != nil {
		return q.enqueueFn(ctx, p)
	}
	payload, _ := json.Marshal(p.Payload)
	j := q.add(&jobs.Job{
		Kind:        p.Kind,
		Payload:     payload,
		Status:      jobs.StatusQueued,
		MaxAttempts: maxAttemptsOrOne(p.MaxAttempts),
		DedupeKey:   nilIfEmpty(p.DedupeKey),
		RunAt:       fixedNow,
		CreatedAt:   fixedNow,
		UpdatedAt:   fixedNow,
	})
	return j, false, nil
}

func (q *stubJobQueue) Get(ctx context.Context, id int64) (*jobs.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	if !ok {
		return nil, jobs.ErrNotFound
	}
	return j, nil
}

func (q *stubJobQueue) List(ctx context.Context, f jobs.ListFilter) ([]*jobs.Job, error) {
	if q.listErr != nil {
		return nil, q.listErr
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []*jobs.Job
	for _, j := range q.jobs {
		if f.Kind != "" && j.Kind != f.Kind {
			continue
		}
		if f.Status != "" && j.Status != f.Status {
			continue
		}
		out = append(out, j)
	}
	return out, nil
}

func (q *stubJobQueue) Cancel(ctx context.Context, id int64, actor, reason string) (jobs.CancelOutcome, *jobs.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	j, ok := q.jobs[id]
	if !ok {
		return "", nil, jobs.ErrNotFound
	}
	switch j.Status {
	case jobs.StatusQueued:
		j.Status = jobs.StatusCanceled
		return jobs.CancelDone, j, nil
	case jobs.StatusRunning:
		j.CancelRequested = true
		return jobs.CancelRequested, j, nil
	default:
		return jobs.CancelAlreadyTerminal, j, nil
	}
}

func maxAttemptsOrOne(n int32) int32 {
	if n < 1 {
		return 1
	}
	return n
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ---------------------------------------------------------------------------
// stubDataStore
// ---------------------------------------------------------------------------

// stubDataStore is an in-memory DataStore. Field values back the read methods;
// the *Err fields inject failures for the 500 paths.
type stubDataStore struct {
	coverage   []TableCoverage
	spans      []TickerSpan
	barDates   map[string][]calendar.Date
	tickers    map[string]bool
	search     []TickerMeta
	watermarks []SyncWatermark
	runs       []SyncRun

	coverageErr error
	searchErr   error
}

func (s *stubDataStore) TableCoverage(ctx context.Context) ([]TableCoverage, error) {
	return s.coverage, s.coverageErr
}
func (s *stubDataStore) BarSpans(ctx context.Context) ([]TickerSpan, error) { return s.spans, nil }
func (s *stubDataStore) BarDates(ctx context.Context, t string) ([]calendar.Date, error) {
	return s.barDates[t], nil
}
func (s *stubDataStore) TickerExists(ctx context.Context, t string) (bool, error) {
	return s.tickers[t], nil
}
func (s *stubDataStore) SearchTickers(ctx context.Context, q string, limit int) ([]TickerMeta, error) {
	if s.searchErr != nil {
		return nil, s.searchErr
	}
	if len(s.search) > limit {
		return s.search[:limit], nil
	}
	return s.search, nil
}
func (s *stubDataStore) SyncWatermarks(ctx context.Context) ([]SyncWatermark, error) {
	return s.watermarks, nil
}
func (s *stubDataStore) SyncRuns(ctx context.Context, dataset string, limit int) ([]SyncRun, error) {
	return s.runs, nil
}

// ---------------------------------------------------------------------------
// stubUniverseReader
// ---------------------------------------------------------------------------

type stubUniverseReader struct {
	snap *universe.Snapshot
	err  error
}

func (s *stubUniverseReader) LatestSnapshot(ctx context.Context, kind string) (*universe.Snapshot, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.snap, nil
}

// stubRunsReader is an in-memory RunsReader for the backtest endpoint contract
// tests. Each field can be set per test; err short-circuits every method.
type stubRunsReader struct {
	list     []runs.RunSummary
	detail   *runs.RunDetail
	equity   []runs.EquitySample
	trades   []runs.TradeRow
	orders   json.RawMessage
	notFound bool
	err      error

	lastListFilter  runs.ListFilter
	lastEquityID    int64
	lastEquityScope string
}

func (s *stubRunsReader) List(_ context.Context, f runs.ListFilter) ([]runs.RunSummary, error) {
	s.lastListFilter = f
	if s.err != nil {
		return nil, s.err
	}
	return s.list, nil
}

func (s *stubRunsReader) Get(_ context.Context, id int64) (*runs.RunDetail, error) {
	if s.notFound {
		return nil, runs.ErrRunNotFound
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.detail, nil
}

func (s *stubRunsReader) Equity(_ context.Context, id int64, scope string) ([]runs.EquitySample, error) {
	s.lastEquityID = id
	s.lastEquityScope = scope
	if s.notFound {
		return nil, runs.ErrRunNotFound
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.equity, nil
}

func (s *stubRunsReader) Trades(_ context.Context, id int64) ([]runs.TradeRow, error) {
	if s.notFound {
		return nil, runs.ErrRunNotFound
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.trades, nil
}

func (s *stubRunsReader) Orders(_ context.Context, id int64) (json.RawMessage, error) {
	if s.notFound {
		return nil, runs.ErrRunNotFound
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.orders, nil
}

// ---------------------------------------------------------------------------
// stubAuditReader
// ---------------------------------------------------------------------------

// stubAuditReader is an in-memory AuditReader. entries are stored newest-first
// (as the PG query returns them); the stub applies the filter + keyset cursor +
// limit so the handler's pagination contract is exercised without a DB.
type stubAuditReader struct {
	entries   []AuditEntry
	err       error
	lastQuery AuditFilter
}

func (s *stubAuditReader) Audit(_ context.Context, f AuditFilter) ([]AuditEntry, error) {
	s.lastQuery = f
	if s.err != nil {
		return nil, s.err
	}
	limit := f.Limit
	if limit < 1 {
		limit = 50
	}
	out := make([]AuditEntry, 0, limit)
	for _, e := range s.entries {
		if f.Actor != "" && e.Actor != f.Actor {
			continue
		}
		if f.Action != "" && e.Action != f.Action {
			continue
		}
		if f.Entity != "" && e.Entity != f.Entity {
			continue
		}
		if f.EntityID != "" && e.EntityID != f.EntityID {
			continue
		}
		if f.Before != nil && e.ID >= *f.Before {
			continue
		}
		out = append(out, e)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// stubSyncForcer
// ---------------------------------------------------------------------------

// stubSyncForcer is an in-memory SyncForcer for the POST /data/sync-now
// contract tests. result is returned (with an incrementing call count);
// err short-circuits to the 500 path.
type stubSyncForcer struct {
	mu        sync.Mutex
	result    SyncNowResult
	err       error
	calls     int
	lastActor string
}

func (s *stubSyncForcer) SyncNow(_ context.Context, actor string) (SyncNowResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastActor = actor
	if s.err != nil {
		return SyncNowResult{}, s.err
	}
	return s.result, nil
}

// ---------------------------------------------------------------------------
// test harness
// ---------------------------------------------------------------------------

// testServer bundles a wired Server with its stubs for assertion access.
// stubModelStore is an in-memory ModelStore seeded from model.SeedModels, so the
// backtest/optimize model_id resolution + the /models CRUD run without a DB.
type stubModelStore struct {
	models map[string]model.Model
	err    error
}

func newStubModelStore() *stubModelStore {
	m := map[string]model.Model{}
	for _, sm := range model.SeedModels() {
		m[sm.ID] = sm
	}
	return &stubModelStore{models: m}
}

func (s *stubModelStore) List(context.Context) ([]model.Model, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make([]model.Model, 0, len(s.models))
	for _, m := range s.models {
		out = append(out, m)
	}
	return out, nil
}

func (s *stubModelStore) Get(_ context.Context, id string) (*model.Model, error) {
	if s.err != nil {
		return nil, s.err
	}
	m, ok := s.models[id]
	if !ok {
		return nil, model.ErrNotFound
	}
	return &m, nil
}

func (s *stubModelStore) Create(_ context.Context, m model.Model) error {
	if s.err != nil {
		return s.err
	}
	if err := m.Validate(); err != nil {
		return err
	}
	s.models[m.ID] = m
	return nil
}

func (s *stubModelStore) Update(_ context.Context, m model.Model) error {
	if s.err != nil {
		return s.err
	}
	if _, ok := s.models[m.ID]; !ok {
		return model.ErrNotFound
	}
	s.models[m.ID] = m
	return nil
}

func (s *stubModelStore) Delete(_ context.Context, id string) error {
	if s.err != nil {
		return s.err
	}
	if _, ok := s.models[id]; !ok {
		return model.ErrNotFound
	}
	delete(s.models, id)
	return nil
}

// stubAuditWriter records the audit rows the Model mutations append.
type stubAuditWriter struct {
	records []AuditRecord
	err     error
}

func (s *stubAuditWriter) WriteAudit(_ context.Context, rec AuditRecord) error {
	if s.err != nil {
		return s.err
	}
	s.records = append(s.records, rec)
	return nil
}

type testServer struct {
	srv      *Server
	jobs     *stubJobQueue
	data     *stubDataStore
	uni      *stubUniverseReader
	runs     *stubRunsReader
	hyperopt *stubHyperoptReader
	promoter *stubPromoter
	audit    *stubAuditReader
	sync     *stubSyncForcer
	models   *stubModelStore
	auditLog *stubAuditWriter
}

// pingOK / pingErr are reusable PingFuncs.
func pingOK(context.Context) error  { return nil }
func pingErr(context.Context) error { return errors.New("connection refused") }

// newTestServer builds a Server over fresh stubs with a fixed clock. Callers
// mutate the returned stubs before issuing requests. pingRedis defaults to OK;
// pass nil-tolerant options through the returned stubs where needed.
func newTestServer(t *testing.T) *testServer {
	t.Helper()
	cal, err := calendar.NewNYSE()
	require.NoError(t, err)

	jq := newStubJobQueue()
	ds := &stubDataStore{barDates: map[string][]calendar.Date{}, tickers: map[string]bool{}}
	ur := &stubUniverseReader{}
	rr := &stubRunsReader{}
	hr := &stubHyperoptReader{}
	pr := &stubPromoter{}
	ar := &stubAuditReader{}
	sf := &stubSyncForcer{result: SyncNowResult{TradingDate: "2024-06-12", Forced: true, DataJobID: 1, EODJobID: 2}}
	ms := newStubModelStore()
	aw := &stubAuditWriter{}

	srv, err := NewServer(Deps{
		Log:         zerolog.Nop(),
		Token:       testToken,
		CORSOrigins: []string{testOrigin},
		Jobs:        jq,
		Data:        ds,
		Universe:    ur,
		Runs:        rr,
		Strategies:  NewStrategyReader(nil, ""),
		Hyperopt:    hr,
		Promoter:    pr,
		Models:      ms,
		AuditLog:    aw,
		Calendar:    cal,
		PingPG:      pingOK,
		PingRedis:   pingOK,
		Audit:       ar,
		Sync:        sf,
		Now:         func() time.Time { return fixedNow },
	})
	require.NoError(t, err)
	return &testServer{srv: srv, jobs: jq, data: ds, uni: ur, runs: rr, hyperopt: hr, promoter: pr, audit: ar, sync: sf, models: ms, auditLog: aw}
}

// do issues a request against the wired router and returns the recorder.
// When auth is true, the bearer token header is attached.
func (ts *testServer) do(t *testing.T, method, target string, body io.Reader, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}
	rec := httptest.NewRecorder()
	ts.srv.Routes().ServeHTTP(rec, req)
	return rec
}

// decodeBody unmarshals the recorder body into a generic map.
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m), "body: %s", rec.Body.String())
	return m
}

// errCode extracts error.code from an error-envelope response.
func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	m := decodeBody(t, rec)
	errObj, ok := m["error"].(map[string]any)
	require.True(t, ok, "expected error envelope, got %s", rec.Body.String())
	return errObj["code"].(string)
}

// doToken issues a request with an arbitrary bearer token (wrong-token tests).
func (ts *testServer) doToken(t *testing.T, method, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	ts.srv.Routes().ServeHTTP(rec, req)
	return rec
}

// doPreflight issues a CORS preflight (OPTIONS) request for the given origin.
func (ts *testServer) doPreflight(t *testing.T, target, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodOptions, target, nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	ts.srv.Routes().ServeHTTP(rec, req)
	return rec
}

// jobPath builds /api/v1/jobs/{id}.
func jobPath(id int64) string {
	return "/api/v1/jobs/" + strconv.FormatInt(id, 10)
}

// assertErr is a stable error value for the *Err injection fields.
func assertErr(msg string) error { return errors.New(msg) }
