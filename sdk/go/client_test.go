package flagsdk_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MahyarNemati/feature-flags/internal/api"
	"github.com/MahyarNemati/feature-flags/internal/flag"
	"github.com/MahyarNemati/feature-flags/internal/store"
	flagsdk "github.com/MahyarNemati/feature-flags/sdk/go"
)

const token = "sdk-token-at-least-16-x"

func serve(t *testing.T) (*httptest.Server, *store.Memory) {
	t.Helper()
	st := store.NewMemory()
	srv := httptest.NewServer(api.New(st, []api.Token{
		{Value: token, Name: "sdk", Admin: true},
	}, nil).Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = st.Close()
	})
	return srv, st
}

func newFlag(key string, enabled bool, onValue any, kind flag.Kind) *flag.Flag {
	return &flag.Flag{
		Key: key, Kind: kind, Enabled: enabled,
		Variations: []flag.Variation{
			{Key: "on", Value: onValue},
			{Key: "off", Value: zeroFor(kind)},
		},
		DefaultVariation: "on",
		OffVariation:     "off",
	}
}

func zeroFor(k flag.Kind) any {
	switch k {
	case flag.KindBool:
		return false
	case flag.KindInt:
		return 0
	case flag.KindJSON:
		return map[string]any{}
	default:
		return ""
	}
}

func client(t *testing.T, srv *httptest.Server, stream bool) *flagsdk.Client {
	t.Helper()
	c, err := flagsdk.New(flagsdk.Options{
		BaseURL:      srv.URL,
		Token:        token,
		PollInterval: 50 * time.Millisecond,
		Stream:       &stream,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	return c
}

func TestTypedAccessors(t *testing.T) {
	srv, st := serve(t)
	ctxb := context.Background()
	_, _ = st.Create(ctxb, newFlag("bool-flag", true, true, flag.KindBool), "test", "")
	_, _ = st.Create(ctxb, newFlag("string-flag", true, "hello", flag.KindString), "test", "")
	_, _ = st.Create(ctxb, newFlag("int-flag", true, 42, flag.KindInt), "test", "")

	c := client(t, srv, false)
	subject := flagsdk.Context{Key: "u1"}

	if got := c.Bool("bool-flag", subject, false); got != true {
		t.Errorf("Bool = %v, want true", got)
	}
	if got := c.String("string-flag", subject, "fallback"); got != "hello" {
		t.Errorf("String = %q, want hello", got)
	}
	if got := c.Int("int-flag", subject, 0); got != 42 {
		t.Errorf("Int = %d, want 42", got)
	}
}

func TestAccessorsReturnFallbackOnTypeMismatch(t *testing.T) {
	srv, st := serve(t)
	_, _ = st.Create(context.Background(), newFlag("string-flag", true, "hello", flag.KindString), "test", "")

	c := client(t, srv, false)
	// Asking for the wrong type must not panic or return a zero value by
	// accident: the caller's fallback is the only safe answer.
	if got := c.Bool("string-flag", flagsdk.Context{Key: "u"}, true); got != true {
		t.Errorf("Bool on a string flag = %v, want the fallback true", got)
	}
}

func TestUnknownFlagReturnsFallback(t *testing.T) {
	srv, _ := serve(t)
	c := client(t, srv, false)

	if got := c.Bool("no-such-flag", flagsdk.Context{Key: "u"}, true); got != true {
		t.Errorf("got %v, want the fallback", got)
	}
	res := c.Evaluate("no-such-flag", flagsdk.Context{Key: "u"}, nil)
	if res.Reason != "flag_not_found" {
		t.Errorf("Reason = %q, want flag_not_found", res.Reason)
	}
}

func TestEvaluationIsLocalAndSurvivesTheServerGoingAway(t *testing.T) {
	// The property that matters most: a flag check must never depend on a
	// network call, so a dead server means stale flags rather than a broken
	// application.
	srv, st := serve(t)
	_, _ = st.Create(context.Background(), newFlag("resilient", true, true, flag.KindBool), "test", "")

	c := client(t, srv, false)
	if got := c.Bool("resilient", flagsdk.Context{Key: "u"}, false); got != true {
		t.Fatalf("precondition failed: got %v", got)
	}

	srv.Close()

	// Still answers, from the cached snapshot.
	for i := 0; i < 50; i++ {
		if got := c.Bool("resilient", flagsdk.Context{Key: "u"}, false); got != true {
			t.Fatalf("evaluation %d failed after the server went away: got %v", i, got)
		}
	}
}

func TestConnectDoesNotFailWhenTheServerIsDown(t *testing.T) {
	// A flag server that is down at deploy time must not stop the
	// application from starting.
	c, err := flagsdk.New(flagsdk.Options{
		BaseURL:        "http://127.0.0.1:1",
		Token:          token,
		InitialTimeout: 200 * time.Millisecond,
		PollInterval:   time.Hour,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect should tolerate an unreachable server, got %v", err)
	}
	if got := c.Bool("anything", flagsdk.Context{Key: "u"}, true); got != true {
		t.Errorf("got %v, want the fallback while disconnected", got)
	}
}

func TestStreamDeliversChangesWithoutPolling(t *testing.T) {
	srv, st := serve(t)
	ctxb := context.Background()
	_, _ = st.Create(ctxb, newFlag("live", false, true, flag.KindBool), "test", "")

	// A very long poll interval, so anything observed must have arrived over
	// the stream.
	stream := true
	c, err := flagsdk.New(flagsdk.Options{
		BaseURL: srv.URL, Token: token,
		PollInterval: time.Hour, Stream: &stream,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.Connect(ctxb); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ready, cancel := context.WithTimeout(ctxb, 5*time.Second)
	defer cancel()
	if err := c.WaitReady(ready); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	if got := c.Bool("live", flagsdk.Context{Key: "u"}, false); got != false {
		t.Fatalf("precondition: flag should start off, got %v", got)
	}

	// Enable it server-side.
	f, _ := st.Get(ctxb, "live")
	f.Enabled = true
	if _, err := st.Update(ctxb, f, f.Version, "test", "enable"); err != nil {
		t.Fatalf("Update: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.Bool("live", flagsdk.Context{Key: "u"}, false) {
			return // arrived over the stream
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the change never arrived over the stream")
}

func TestPollingPicksUpChangesWhenStreamingIsOff(t *testing.T) {
	srv, st := serve(t)
	ctxb := context.Background()
	_, _ = st.Create(ctxb, newFlag("polled", false, true, flag.KindBool), "test", "")

	c := client(t, srv, false) // 50ms poll, no stream

	f, _ := st.Get(ctxb, "polled")
	f.Enabled = true
	_, _ = st.Update(ctxb, f, f.Version, "test", "enable")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.Bool("polled", flagsdk.Context{Key: "u"}, false) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("polling never picked up the change")
}

func TestJSONFlagUnmarshalsIntoAStruct(t *testing.T) {
	srv, st := serve(t)
	cfg := map[string]any{"retries": float64(3), "mode": "fast"}
	_, _ = st.Create(context.Background(), newFlag("config", true, cfg, flag.KindJSON), "test", "")

	c := client(t, srv, false)

	var out struct {
		Retries int    `json:"retries"`
		Mode    string `json:"mode"`
	}
	if !c.JSON("config", flagsdk.Context{Key: "u"}, &out) {
		t.Fatal("JSON returned false")
	}
	if out.Retries != 3 || out.Mode != "fast" {
		t.Errorf("got %+v", out)
	}

	// A missing flag must leave dst untouched and report false, so the
	// caller's existing value stands in as the fallback.
	out.Mode = "untouched"
	if c.JSON("no-such-flag", flagsdk.Context{Key: "u"}, &out) {
		t.Error("JSON should return false for a missing flag")
	}
	if out.Mode != "untouched" {
		t.Errorf("dst was modified on failure: %+v", out)
	}
}

func TestSnapshotReportsWhatTheClientKnows(t *testing.T) {
	srv, st := serve(t)
	_, _ = st.Create(context.Background(), newFlag("one", true, true, flag.KindBool), "test", "")

	c := client(t, srv, false)
	revision, count, age := c.Snapshot()
	if revision == 0 {
		t.Error("revision should be non-zero after a successful fetch")
	}
	if count != 1 {
		t.Errorf("flags = %d, want 1", count)
	}
	if age < 0 || age > time.Minute {
		t.Errorf("age = %s, want something small", age)
	}
}

func TestNewRequiresABaseURL(t *testing.T) {
	if _, err := flagsdk.New(flagsdk.Options{Token: token}); err == nil {
		t.Error("expected an error when BaseURL is empty")
	}
}

func TestConcurrentEvaluationIsRaceFree(t *testing.T) {
	srv, st := serve(t)
	ctxb := context.Background()
	_, _ = st.Create(ctxb, newFlag("hot", true, true, flag.KindBool), "test", "")

	c := client(t, srv, true)

	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 500; j++ {
				c.Bool("hot", flagsdk.Context{Key: "u"}, false)
			}
		}()
	}
	// Mutate server-side while readers run, so the snapshot swap races with
	// evaluation. The atomic pointer is what makes this safe.
	for i := 0; i < 20; i++ {
		f, _ := st.Get(ctxb, "hot")
		_, _ = st.Update(ctxb, f, f.Version, "test", "churn")
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
