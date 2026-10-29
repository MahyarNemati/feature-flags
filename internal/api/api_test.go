package api_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MahyarNemati/feature-flags/internal/api"
	"github.com/MahyarNemati/feature-flags/internal/flag"
	"github.com/MahyarNemati/feature-flags/internal/store"
)

const (
	adminToken = "admin-token-at-least-16"
	sdkToken   = "sdk-token-at-least-16-x"
)

func harness(t *testing.T) (*httptest.Server, *store.Memory) {
	t.Helper()
	st := store.NewMemory()
	srv := httptest.NewServer(api.New(st, []api.Token{
		{Value: adminToken, Name: "admin", Admin: true},
		{Value: sdkToken, Name: "sdk", Admin: false},
	}, nil).Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = st.Close()
	})
	return srv, st
}

func do(t *testing.T, srv *httptest.Server, method, path, token string, body any, headers ...[2]string) *http.Response {
	t.Helper()
	var r *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		r = bytes.NewReader(raw)
	} else {
		r = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, srv.URL+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decode(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func boolFlagBody(key string, enabled bool) map[string]any {
	return map[string]any{
		"key":     key,
		"kind":    "bool",
		"enabled": enabled,
		"variations": []map[string]any{
			{"key": "on", "value": true},
			{"key": "off", "value": false},
		},
		"default_variation": "off",
		"off_variation":     "off",
		"comment":           "created in a test",
	}
}

// ---------- auth ----------

func TestUnauthenticatedIsRejected(t *testing.T) {
	srv, _ := harness(t)
	if resp := do(t, srv, http.MethodGet, "/api/v1/flags", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("got %d, want 401", resp.StatusCode)
	}
}

func TestBadTokenIsRejected(t *testing.T) {
	srv, _ := harness(t)
	if resp := do(t, srv, http.MethodGet, "/api/v1/flags", "nope", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("got %d, want 401", resp.StatusCode)
	}
}

func TestSDKTokenMayEvaluateButNotMutate(t *testing.T) {
	srv, _ := harness(t)

	// Read is allowed.
	if resp := do(t, srv, http.MethodGet, "/api/v1/flags", sdkToken, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("list with the SDK token got %d, want 200", resp.StatusCode)
	}
	// Write is not.
	resp := do(t, srv, http.MethodPost, "/api/v1/flags", sdkToken, boolFlagBody("x-flag", true))
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("create with the SDK token got %d, want 403", resp.StatusCode)
	}
}

func TestNoTokensConfiguredDisablesTheAPI(t *testing.T) {
	st := store.NewMemory()
	defer st.Close()
	srv := httptest.NewServer(api.New(st, nil, nil).Handler())
	defer srv.Close()

	// "No tokens" must mean closed, not open: a flag service anyone can write
	// to lets anyone turn features on in production.
	resp := do(t, srv, http.MethodGet, "/api/v1/flags", "", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("got %d, want 403 when no tokens are configured", resp.StatusCode)
	}
}

// ---------- CRUD ----------

func TestCreateGetUpdateDelete(t *testing.T) {
	srv, _ := harness(t)

	resp := do(t, srv, http.MethodPost, "/api/v1/flags", adminToken, boolFlagBody("checkout", false))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create got %d", resp.StatusCode)
	}
	var created flag.Flag
	decode(t, resp, &created)
	if created.Version != 1 {
		t.Errorf("Version = %d, want 1", created.Version)
	}

	resp = do(t, srv, http.MethodGet, "/api/v1/flags/checkout", sdkToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get got %d", resp.StatusCode)
	}
	if etag := resp.Header.Get("ETag"); etag != `"1"` {
		t.Errorf("ETag = %q, want \"1\"", etag)
	}

	body := boolFlagBody("checkout", true)
	resp = do(t, srv, http.MethodPut, "/api/v1/flags/checkout", adminToken, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update got %d", resp.StatusCode)
	}
	var updated flag.Flag
	decode(t, resp, &updated)
	if updated.Version != 2 || !updated.Enabled {
		t.Errorf("after update: version %d enabled %v", updated.Version, updated.Enabled)
	}

	resp = do(t, srv, http.MethodDelete, "/api/v1/flags/checkout", adminToken, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete got %d", resp.StatusCode)
	}
	if resp := do(t, srv, http.MethodGet, "/api/v1/flags/checkout", sdkToken, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("get after delete got %d, want 404", resp.StatusCode)
	}
}

func TestDuplicateKeyIsAConflict(t *testing.T) {
	srv, _ := harness(t)
	do(t, srv, http.MethodPost, "/api/v1/flags", adminToken, boolFlagBody("dup", true))
	resp := do(t, srv, http.MethodPost, "/api/v1/flags", adminToken, boolFlagBody("dup", true))
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("got %d, want 409", resp.StatusCode)
	}
}

func TestInvalidFlagIsABadRequestWithAUsefulMessage(t *testing.T) {
	srv, _ := harness(t)
	bad := boolFlagBody("broken", true)
	bad["default_variation"] = "missing"

	resp := do(t, srv, http.MethodPost, "/api/v1/flags", adminToken, bad)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", resp.StatusCode)
	}
	var body map[string]string
	decode(t, resp, &body)
	if !strings.Contains(body["error"], "default_variation") {
		t.Errorf("error should name the problem, got %q", body["error"])
	}
}

func TestKeyMismatchBetweenBodyAndURLIsRejected(t *testing.T) {
	srv, _ := harness(t)
	do(t, srv, http.MethodPost, "/api/v1/flags", adminToken, boolFlagBody("real", true))

	body := boolFlagBody("different", true)
	resp := do(t, srv, http.MethodPut, "/api/v1/flags/real", adminToken, body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("got %d, want 400 for a key mismatch", resp.StatusCode)
	}
}

func TestIfMatchPreventsLostUpdates(t *testing.T) {
	srv, _ := harness(t)
	do(t, srv, http.MethodPost, "/api/v1/flags", adminToken, boolFlagBody("race", false))

	// Two people read version 1; the first write wins.
	first := do(t, srv, http.MethodPut, "/api/v1/flags/race", adminToken,
		boolFlagBody("race", true), [2]string{"If-Match", `"1"`})
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first update got %d", first.StatusCode)
	}
	second := do(t, srv, http.MethodPut, "/api/v1/flags/race", adminToken,
		boolFlagBody("race", false), [2]string{"If-Match", `"1"`})
	if second.StatusCode != http.StatusPreconditionFailed {
		t.Errorf("second update got %d, want 412: the loser must be told", second.StatusCode)
	}
}

func TestUnknownFieldsAreRejected(t *testing.T) {
	srv, _ := harness(t)
	body := boolFlagBody("typo", true)
	body["enabledd"] = true

	resp := do(t, srv, http.MethodPost, "/api/v1/flags", adminToken, body)
	// A typo'd field that is silently ignored is a flag that does not behave
	// the way its author believes it does.
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("got %d, want 400 for an unknown field", resp.StatusCode)
	}
}

// ---------- evaluation ----------

func TestEvaluateReturnsVariationAndReason(t *testing.T) {
	srv, _ := harness(t)
	do(t, srv, http.MethodPost, "/api/v1/flags", adminToken, boolFlagBody("feature", true))

	resp := do(t, srv, http.MethodPost, "/api/v1/evaluate", sdkToken, map[string]any{
		"flag_key": "feature",
		"context":  map[string]any{"key": "user-1"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
	var res struct {
		Variation string `json:"variation"`
		Value     any    `json:"value"`
		Reason    string `json:"reason"`
	}
	decode(t, resp, &res)
	if res.Reason != "default" || res.Variation != "off" {
		t.Errorf("got %+v, want the default variation", res)
	}
}

func TestEvaluateMissingFlagIsTwoHundredWithFallback(t *testing.T) {
	srv, _ := harness(t)
	resp := do(t, srv, http.MethodPost, "/api/v1/evaluate", sdkToken, map[string]any{
		"flag_key": "nope",
		"context":  map[string]any{"key": "u"},
		"fallback": "my-default",
	})
	// Not a 404: the caller asked what to serve, and the answer is their
	// fallback. Making it an error forces every call site to handle a
	// condition with an obvious correct behaviour.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}
	var res struct {
		Value  any    `json:"value"`
		Reason string `json:"reason"`
	}
	decode(t, resp, &res)
	if res.Reason != "flag_not_found" || res.Value != "my-default" {
		t.Errorf("got %+v, want the fallback and flag_not_found", res)
	}
}

func TestEvaluateAll(t *testing.T) {
	srv, _ := harness(t)
	do(t, srv, http.MethodPost, "/api/v1/flags", adminToken, boolFlagBody("flag-a", true))
	do(t, srv, http.MethodPost, "/api/v1/flags", adminToken, boolFlagBody("flag-b", false))

	resp := do(t, srv, http.MethodPost, "/api/v1/evaluate/all", sdkToken, map[string]any{
		"context": map[string]any{"key": "u"},
	})
	var body struct {
		Evaluations map[string]struct {
			Reason string `json:"reason"`
		} `json:"evaluations"`
	}
	decode(t, resp, &body)
	if len(body.Evaluations) != 2 {
		t.Fatalf("got %d evaluations, want 2", len(body.Evaluations))
	}
	if body.Evaluations["flag-b"].Reason != "off" {
		t.Errorf("flag-b reason = %q, want off", body.Evaluations["flag-b"].Reason)
	}
}

// ---------- snapshot and streaming ----------

func TestSnapshotETagAvoidsResendingUnchangedFlags(t *testing.T) {
	srv, _ := harness(t)
	do(t, srv, http.MethodPost, "/api/v1/flags", adminToken, boolFlagBody("flag-a", true))

	resp := do(t, srv, http.MethodGet, "/api/v1/snapshot", sdkToken, nil)
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("snapshot should carry an ETag")
	}

	again := do(t, srv, http.MethodGet, "/api/v1/snapshot", sdkToken, nil,
		[2]string{"If-None-Match", etag})
	if again.StatusCode != http.StatusNotModified {
		t.Errorf("got %d, want 304 for an unchanged snapshot", again.StatusCode)
	}

	// A change must invalidate it.
	do(t, srv, http.MethodPost, "/api/v1/flags", adminToken, boolFlagBody("flag-b", true))
	third := do(t, srv, http.MethodGet, "/api/v1/snapshot", sdkToken, nil,
		[2]string{"If-None-Match", etag})
	if third.StatusCode != http.StatusOK {
		t.Errorf("got %d, want 200 after a change", third.StatusCode)
	}
}

func TestStreamSendsCurrentStateImmediatelyThenOnChange(t *testing.T) {
	srv, _ := harness(t)
	do(t, srv, http.MethodPost, "/api/v1/flags", adminToken, boolFlagBody("streamed", true))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/stream", nil)
	req.Header.Set("Authorization", "Bearer "+sdkToken)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}

	events := make(chan map[string]any, 4)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		var data strings.Builder
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "data:"):
				data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			case line == "" && data.Len() > 0:
				var m map[string]any
				if json.Unmarshal([]byte(data.String()), &m) == nil {
					select {
					case events <- m:
					default:
					}
				}
				data.Reset()
			}
		}
	}()

	// A client must be correct from the first byte, not after the first
	// change, or it serves fallbacks until something happens to be edited.
	select {
	case first := <-events:
		flags, _ := first["flags"].(map[string]any)
		if _, ok := flags["streamed"]; !ok {
			t.Errorf("first event should contain the current flags, got %v", first)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no initial event within 5s")
	}

	// Now change something and expect a push.
	do(t, srv, http.MethodPost, "/api/v1/flags", adminToken, boolFlagBody("added-later", true))
	select {
	case next := <-events:
		flags, _ := next["flags"].(map[string]any)
		if _, ok := flags["added-later"]; !ok {
			t.Errorf("expected the new flag in the pushed event, got %v", next)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event pushed after a change")
	}
}

// ---------- audit ----------

func TestHistoryRecordsActorActionAndComment(t *testing.T) {
	srv, _ := harness(t)

	body := boolFlagBody("audited", false)
	body["comment"] = "initial creation"
	do(t, srv, http.MethodPost, "/api/v1/flags", adminToken, body)

	enable := boolFlagBody("audited", true)
	enable["comment"] = "ramping up"
	do(t, srv, http.MethodPut, "/api/v1/flags/audited", adminToken, enable)

	resp := do(t, srv, http.MethodGet, "/api/v1/flags/audited/history", sdkToken, nil)
	var out struct {
		Changes []struct {
			Action  string `json:"action"`
			Actor   string `json:"actor"`
			Comment string `json:"comment"`
		} `json:"changes"`
	}
	decode(t, resp, &out)

	if len(out.Changes) != 2 {
		t.Fatalf("got %d changes, want 2", len(out.Changes))
	}
	// Newest first: the question is almost always "what changed recently".
	if out.Changes[0].Action != "enable" {
		t.Errorf("latest action = %q, want enable", out.Changes[0].Action)
	}
	if out.Changes[0].Actor != "admin" {
		t.Errorf("actor = %q, want the token's name", out.Changes[0].Actor)
	}
	if out.Changes[0].Comment != "ramping up" {
		t.Errorf("comment = %q", out.Changes[0].Comment)
	}
}

func TestHistoryDistinguishesEnableFromGenericUpdate(t *testing.T) {
	srv, _ := harness(t)
	do(t, srv, http.MethodPost, "/api/v1/flags", adminToken, boolFlagBody("sw", true))

	off := boolFlagBody("sw", false)
	do(t, srv, http.MethodPut, "/api/v1/flags/sw", adminToken, off)

	resp := do(t, srv, http.MethodGet, "/api/v1/history", sdkToken, nil)
	var out struct {
		Changes []struct {
			Action string `json:"action"`
		} `json:"changes"`
	}
	decode(t, resp, &out)
	if out.Changes[0].Action != "disable" {
		t.Errorf("action = %q, want disable; a kill-switch flip should be obvious in the log", out.Changes[0].Action)
	}
}

func TestHistoryLimitIsValidated(t *testing.T) {
	srv, _ := harness(t)
	for _, limit := range []string{"0", "-1", "1000", "abc"} {
		resp := do(t, srv, http.MethodGet, "/api/v1/history?limit="+limit, sdkToken, nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("limit=%s got %d, want 400", limit, resp.StatusCode)
		}
	}
}

// ---------- health ----------

func TestHealthAndReadiness(t *testing.T) {
	srv, _ := harness(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		if resp := do(t, srv, http.MethodGet, path, "", nil); resp.StatusCode != http.StatusOK {
			t.Errorf("%s got %d, want 200 (health must not need a token)", path, resp.StatusCode)
		}
	}
}

func TestListExcludesArchivedByDefault(t *testing.T) {
	srv, _ := harness(t)
	body := boolFlagBody("old", true)
	do(t, srv, http.MethodPost, "/api/v1/flags", adminToken, body)

	archived := boolFlagBody("old", true)
	archived["archived"] = true
	do(t, srv, http.MethodPut, "/api/v1/flags/old", adminToken, archived)

	var out struct {
		Flags []flag.Flag `json:"flags"`
	}
	decode(t, do(t, srv, http.MethodGet, "/api/v1/flags", sdkToken, nil), &out)
	if len(out.Flags) != 0 {
		t.Errorf("archived flag should be hidden by default, got %d", len(out.Flags))
	}

	var all struct {
		Flags []flag.Flag `json:"flags"`
	}
	decode(t, do(t, srv, http.MethodGet, "/api/v1/flags?archived=true", sdkToken, nil), &all)
	if len(all.Flags) != 1 {
		t.Errorf("archived=true should include it, got %d", len(all.Flags))
	}

	// Crucially, it still evaluates: archiving must not break production.
	resp := do(t, srv, http.MethodPost, "/api/v1/evaluate", sdkToken, map[string]any{
		"flag_key": "old", "context": map[string]any{"key": "u"},
	})
	var res struct {
		Reason string `json:"reason"`
	}
	decode(t, resp, &res)
	if res.Reason == "flag_not_found" {
		t.Error("an archived flag must still evaluate, or archiving breaks running code")
	}
}

func TestConcurrentEvaluationIsSafe(t *testing.T) {
	srv, _ := harness(t)
	do(t, srv, http.MethodPost, "/api/v1/flags", adminToken, boolFlagBody("hot", true))

	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 25; j++ {
				resp := do(t, srv, http.MethodPost, "/api/v1/evaluate", sdkToken, map[string]any{
					"flag_key": "hot",
					"context":  map[string]any{"key": fmt.Sprintf("u-%d-%d", n, j)},
				})
				if resp.StatusCode != http.StatusOK {
					t.Errorf("evaluate got %d", resp.StatusCode)
					return
				}
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

func TestSingleCharacterKeyIsRejected(t *testing.T) {
	// Worth asserting explicitly: two of the tests above were originally
	// written with one-character keys and silently asserted against an empty
	// store, because create returned 400 and nothing checked it.
	srv, _ := harness(t)
	resp := do(t, srv, http.MethodPost, "/api/v1/flags", adminToken, boolFlagBody("a", true))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("got %d, want 400 for a one-character key", resp.StatusCode)
	}
}
