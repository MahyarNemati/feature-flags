// Package api serves the admin REST API, the evaluation endpoints and the
// streaming endpoint SDKs use to stay current.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MahyarNemati/feature-flags/internal/eval"
	"github.com/MahyarNemati/feature-flags/internal/flag"
	"github.com/MahyarNemati/feature-flags/internal/store"
)

// Token is an API credential with a role.
type Token struct {
	// Value is the bearer token.
	Value string
	// Name identifies the holder in the audit log, which is the whole point
	// of having named tokens rather than one shared secret.
	Name string
	// Admin tokens may mutate flags; non-admin tokens may only evaluate.
	Admin bool
}

// Server serves the API.
type Server struct {
	store  store.Store
	subs   Subscriber
	tokens []Token
	log    *slog.Logger
	// streamKeepalive bounds how long a stream can be idle before a comment
	// is sent, so proxies do not time the connection out.
	streamKeepalive time.Duration
}

// Subscriber is implemented by stores that can signal changes, enabling the
// streaming endpoint. A store without it still works; clients poll instead.
type Subscriber interface {
	Subscribe() (<-chan struct{}, func())
}

// New builds a Server.
func New(st store.Store, tokens []Token, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{store: st, tokens: tokens, log: log, streamKeepalive: 25 * time.Second}
	if sub, ok := st.(Subscriber); ok {
		s.subs = sub
	}
	return s
}

// Handler returns the router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Evaluation: any valid token.
	mux.Handle("POST /api/v1/evaluate", s.authed(false, http.HandlerFunc(s.handleEvaluate)))
	mux.Handle("POST /api/v1/evaluate/all", s.authed(false, http.HandlerFunc(s.handleEvaluateAll)))
	mux.Handle("GET /api/v1/snapshot", s.authed(false, http.HandlerFunc(s.handleSnapshot)))
	mux.Handle("GET /api/v1/stream", s.authed(false, http.HandlerFunc(s.handleStream)))

	// Administration: admin tokens only.
	mux.Handle("GET /api/v1/flags", s.authed(false, http.HandlerFunc(s.handleList)))
	mux.Handle("GET /api/v1/flags/{key}", s.authed(false, http.HandlerFunc(s.handleGet)))
	mux.Handle("POST /api/v1/flags", s.authed(true, http.HandlerFunc(s.handleCreate)))
	mux.Handle("PUT /api/v1/flags/{key}", s.authed(true, http.HandlerFunc(s.handleUpdate)))
	mux.Handle("DELETE /api/v1/flags/{key}", s.authed(true, http.HandlerFunc(s.handleDelete)))
	mux.Handle("GET /api/v1/flags/{key}/history", s.authed(false, http.HandlerFunc(s.handleHistory)))
	mux.Handle("GET /api/v1/history", s.authed(false, http.HandlerFunc(s.handleHistory)))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.handleReady)

	return mux
}

// ---------- auth ----------

type actorKey struct{}

// authed authenticates a bearer token and, when admin is true, requires the
// admin role.
func (s *Server) authed(admin bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.tokens) == 0 {
			// No tokens configured means the API is unauthenticated. Refusing
			// is the safe reading: a flag service with no auth lets anyone
			// turn features on in production.
			writeError(w, http.StatusForbidden, "no API tokens configured; the API is disabled")
			return
		}
		supplied := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if supplied == "" {
			supplied = r.Header.Get("X-API-Key")
		}

		var matched *Token
		for i := range s.tokens {
			// Constant-time compare so a token cannot be recovered by timing.
			if subtle.ConstantTimeCompare([]byte(supplied), []byte(s.tokens[i].Value)) == 1 {
				matched = &s.tokens[i]
				break
			}
		}
		if matched == nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="feature-flags"`)
			writeError(w, http.StatusUnauthorized, "invalid API token")
			return
		}
		if admin && !matched.Admin {
			writeError(w, http.StatusForbidden, "this token may not modify flags")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, matched.Name)))
	})
}

func actor(r *http.Request) string {
	if v, ok := r.Context().Value(actorKey{}).(string); ok {
		return v
	}
	return "unknown"
}

// ---------- evaluation ----------

type evaluateRequest struct {
	FlagKey string       `json:"flag_key"`
	Context eval.Context `json:"context"`
	// Fallback is returned when the flag cannot be evaluated.
	Fallback any `json:"fallback,omitempty"`
}

func (s *Server) handleEvaluate(w http.ResponseWriter, r *http.Request) {
	var req evaluateRequest
	if !decode(w, r, &req) {
		return
	}
	if req.FlagKey == "" {
		writeError(w, http.StatusBadRequest, "flag_key is required")
		return
	}

	f, err := s.store.Get(r.Context(), req.FlagKey)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Error("get flag", "key", req.FlagKey, "err", err)
		writeError(w, http.StatusInternalServerError, "could not load flag")
		return
	}
	// A missing flag is a 200 with reason flag_not_found, not a 404. The
	// caller asked "what should I serve?" and the answer is "your fallback" --
	// making it an error forces every call site to handle a condition that
	// has an obvious correct behaviour.
	writeJSON(w, http.StatusOK, eval.Evaluate(f, req.Context, req.Fallback))
}

type evaluateAllRequest struct {
	Context eval.Context `json:"context"`
	// Keys limits evaluation to these flags; empty means all.
	Keys []string `json:"keys,omitempty"`
}

func (s *Server) handleEvaluateAll(w http.ResponseWriter, r *http.Request) {
	var req evaluateAllRequest
	if !decode(w, r, &req) {
		return
	}

	flags, err := s.store.List(r.Context(), true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list flags")
		return
	}

	wanted := map[string]bool{}
	for _, k := range req.Keys {
		wanted[k] = true
	}

	out := make(map[string]eval.Result, len(flags))
	for _, f := range flags {
		if len(wanted) > 0 && !wanted[f.Key] {
			continue
		}
		out[f.Key] = eval.Evaluate(f, req.Context, nil)
	}
	writeJSON(w, http.StatusOK, map[string]any{"evaluations": out})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	flags, revision, err := s.store.Snapshot(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not build snapshot")
		return
	}
	w.Header().Set("ETag", fmt.Sprintf(`"%d"`, revision))
	// Let a client skip the body when nothing has changed.
	if match := r.Header.Get("If-None-Match"); match == fmt.Sprintf(`"%d"`, revision) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": revision, "flags": flags})
}

// handleStream pushes the whole flag set over Server-Sent Events whenever
// anything changes.
//
// SSE rather than WebSockets because the traffic is one-directional and SSE
// reconnects automatically, survives proxies that only understand HTTP, and
// needs no extra protocol handling in an SDK.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported by this server")
		return
	}
	if s.subs == nil {
		writeError(w, http.StatusNotImplemented, "this store does not support streaming; poll /api/v1/snapshot instead")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	// Nginx buffers proxied responses by default, which breaks SSE entirely.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	changes, unsubscribe := s.subs.Subscribe()
	defer unsubscribe()

	send := func() bool {
		flags, revision, err := s.store.Snapshot(r.Context())
		if err != nil {
			return false
		}
		payload, err := json.Marshal(map[string]any{"revision": revision, "flags": flags})
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: flags\ndata: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// Send the current state immediately, so a client is correct from the
	// first byte rather than after the first change.
	if !send() {
		return
	}

	keepalive := time.NewTicker(s.streamKeepalive)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case _, open := <-changes:
			if !open {
				return
			}
			if !send() {
				return
			}
		case <-keepalive.C:
			// An SSE comment: keeps idle connections alive through proxies
			// without the client having to interpret anything.
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ---------- administration ----------

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	includeArchived := r.URL.Query().Get("archived") == "true"
	flags, err := s.store.List(r.Context(), includeArchived)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list flags")
		return
	}
	if flags == nil {
		flags = []*flag.Flag{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"flags": flags})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	f, err := s.store.Get(r.Context(), r.PathValue("key"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such flag")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load flag")
		return
	}
	w.Header().Set("ETag", fmt.Sprintf(`"%d"`, f.Version))
	writeJSON(w, http.StatusOK, f)
}

type writeRequest struct {
	flag.Flag
	// Comment is recorded in the audit log.
	Comment string `json:"comment,omitempty"`
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req writeRequest
	if !decode(w, r, &req) {
		return
	}
	created, err := s.store.Create(r.Context(), &req.Flag, actor(r), req.Comment)
	switch {
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "a flag with that key already exists")
	case errors.Is(err, flag.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case err != nil:
		s.log.Error("create flag", "err", err)
		writeError(w, http.StatusInternalServerError, "could not create flag")
	default:
		s.log.Info("flag created", "key", created.Key, "actor", actor(r))
		writeJSON(w, http.StatusCreated, created)
	}
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var req writeRequest
	if !decode(w, r, &req) {
		return
	}
	key := r.PathValue("key")
	if req.Key == "" {
		req.Key = key
	}
	if req.Key != key {
		writeError(w, http.StatusBadRequest, "flag key in the body does not match the URL")
		return
	}

	// If-Match carries the expected version, which is how two dashboard users
	// editing at once get a conflict instead of silently overwriting.
	expect := 0
	if raw := strings.Trim(r.Header.Get("If-Match"), `"`); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "If-Match must be a version number")
			return
		}
		expect = n
	}

	updated, err := s.store.Update(r.Context(), &req.Flag, expect, actor(r), req.Comment)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such flag")
	case errors.Is(err, store.ErrStaleVersion):
		writeError(w, http.StatusPreconditionFailed, "the flag changed since you read it; re-read and retry")
	case errors.Is(err, flag.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case err != nil:
		s.log.Error("update flag", "err", err)
		writeError(w, http.StatusInternalServerError, "could not update flag")
	default:
		s.log.Info("flag updated", "key", updated.Key, "version", updated.Version, "actor", actor(r))
		writeJSON(w, http.StatusOK, updated)
	}
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	comment := r.URL.Query().Get("comment")
	err := s.store.Delete(r.Context(), r.PathValue("key"), actor(r), comment)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such flag")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not delete flag")
	default:
		s.log.Info("flag deleted", "key", r.PathValue("key"), "actor", actor(r))
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 500")
			return
		}
		limit = n
	}
	changes, err := s.store.History(r.Context(), r.PathValue("key"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load history")
		return
	}
	if changes == nil {
		changes = []store.Change{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": changes})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// ---------- helpers ----------

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
