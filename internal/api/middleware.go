package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
)

// requestLogger is a zerolog-backed structured access logger. The bearer
// token never appears in logs (only method/path/status/latency).
func requestLogger(log zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			log.Info().
				Str("method", r.Method).
				Str("path", r.URL.Path).
				Int("status", ww.Status()).
				Int("bytes", ww.BytesWritten()).
				Dur("duration", time.Since(start)).
				Str("remote", r.RemoteAddr).
				Str("request_id", middleware.GetReqID(r.Context())).
				Msg("http request")
		})
	}
}

// recoverer converts handler panics into structured 500s (with stack at
// error level) instead of killing the connection. WebSocket-upgraded
// responses cannot take a status anymore; the write is best-effort.
func recoverer(log zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if rec == http.ErrAbortHandler { //nolint:errorlint // sentinel by contract
						panic(rec)
					}
					log.Error().
						Interface("panic", rec).
						Str("path", r.URL.Path).
						Str("request_id", middleware.GetReqID(r.Context())).
						Bytes("stack", debug.Stack()).
						Msg("handler panicked")
					writeError(w, http.StatusInternalServerError, CodeInternal, "internal error; see server logs")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken extracts the credential: the Authorization: Bearer header,
// or — for browser WebSocket clients, which cannot set headers — the
// ?token= query parameter.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if rest, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(rest)
	}
	return r.URL.Query().Get("token")
}

// tokenEqual compares credentials in constant time (hashing first equalizes
// lengths so the comparison leaks neither bytes nor length).
func tokenEqual(got, want string) bool {
	g := sha256.Sum256([]byte(got))
	w := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1
}

// requireAuth guards every /api/* route with the static bearer token.
func requireAuth(token string, log zerolog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := bearerToken(r)
			if got == "" || !tokenEqual(got, token) {
				log.Warn().
					Str("path", r.URL.Path).
					Str("remote", r.RemoteAddr).
					Bool("credential_present", got != "").
					Msg("unauthorized request")
				w.Header().Set("WWW-Authenticate", `Bearer realm="tms-api"`)
				writeError(w, http.StatusUnauthorized, CodeUnauthorized, "missing or invalid bearer token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// corsMiddleware implements the allowlist CORS policy: echo the matching
// Origin, no credentials (per docs/spec/api-ws-redis.md §1.2), preflight
// answered with 204. Non-allowlisted origins get no CORS headers — the
// browser enforces the block.
func corsMiddleware(origins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(origins))
	for _, o := range origins {
		allowed[strings.TrimRight(o, "/")] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" {
				if _, ok := allowed[strings.TrimRight(origin, "/")]; ok {
					h := w.Header()
					h.Set("Access-Control-Allow-Origin", origin)
					h.Add("Vary", "Origin")
					h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
					h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
					h.Set("Access-Control-Max-Age", "600")
				}
			}
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
