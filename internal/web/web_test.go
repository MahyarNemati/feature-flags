package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MahyarNemati/feature-flags/internal/flag"
	"github.com/MahyarNemati/feature-flags/internal/store"
	"github.com/MahyarNemati/feature-flags/internal/web"
)

func serve(t *testing.T, adminToken string) (*httptest.Server, *store.Memory) {
	t.Helper()
	st := store.NewMemory()
	_, err := st.Create(context.Background(), &flag.Flag{
		Key: "new-checkout", Name: "New checkout", Description: "A rebuilt flow",
		Kind: flag.KindBool, Enabled: false,
		Variations: []flag.Variation{
			{Key: "on", Value: true}, {Key: "off", Value: false},
		},
		DefaultVariation: "off", OffVariation: "off",
		Rules: []flag.Rule{{
			ID:         "ramp",
			Conditions: []flag.Condition{{Attribute: "country", Operator: flag.OpIn, Values: []string{"GB"}}},
			Rollout:    &flag.Rollout{Weights: map[string]int{"on": 1000, "off": 9000}},
		}},
	}, "test", "seeded")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	srv, err := web.New(st, adminToken, nil)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	mux := http.NewServeMux()
	srv.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(func() {
		ts.Close()
		_ = st.Close()
	})
	return ts, st
}

func body(t *testing.T, ts *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

// TestEachPageRendersItsOwnContent is a regression test for a real bug: all
// three page files define a template called "content", and when they share a
// single template set the last definition parsed wins -- so every page
// rendered the index. CI caught it; nothing in the unit tests did, because
// there were none for this package.
func TestEachPageRendersItsOwnContent(t *testing.T) {
	ts, _ := serve(t, "")

	code, index := body(t, ts, "/")
	if code != http.StatusOK {
		t.Fatalf("index got %d", code)
	}
	if !strings.Contains(index, "new-checkout") {
		t.Errorf("index does not list the flag:\n%.400s", index)
	}

	code, detail := body(t, ts, "/flags/new-checkout")
	if code != http.StatusOK {
		t.Fatalf("detail got %d", code)
	}
	for _, want := range []string{"Variations", "Targeting rules", "ramp", "Change history"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail page is missing %q (it may be rendering the index):\n%.400s", want, detail)
		}
	}
	// The decisive check: the detail page must not be the index.
	if strings.Contains(detail, "Recent changes") && !strings.Contains(detail, "Variations") {
		t.Error("the detail page rendered the index's content")
	}

	code, history := body(t, ts, "/history")
	if code != http.StatusOK {
		t.Fatalf("history got %d", code)
	}
	if !strings.Contains(history, "seeded") {
		t.Errorf("history page does not show the change:\n%.400s", history)
	}
}

func TestDetailShowsRolloutPercentages(t *testing.T) {
	ts, _ := serve(t, "")
	_, detail := body(t, ts, "/flags/new-checkout")
	// Basis points must be rendered as a human percentage, or the dashboard
	// shows "1000" for 10%.
	if !strings.Contains(detail, "10%") {
		t.Errorf("expected 1000 basis points to render as 10%%:\n%.600s", detail)
	}
}

func TestUnknownFlagIsNotFound(t *testing.T) {
	ts, _ := serve(t, "")
	if code, _ := body(t, ts, "/flags/nope"); code != http.StatusNotFound {
		t.Errorf("got %d, want 404", code)
	}
}

func TestDashboardIsReadOnlyWithoutAnAdminToken(t *testing.T) {
	ts, _ := serve(t, "")
	_, index := body(t, ts, "/")
	if !strings.Contains(index, "Read-only") {
		t.Errorf("expected a read-only notice when no admin token is configured")
	}

	// And the toggle must refuse rather than being quietly available.
	resp, err := ts.Client().PostForm(ts.URL+"/flags/new-checkout/toggle", nil)
	if err != nil {
		t.Fatalf("POST toggle: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("toggle got %d, want 403 on a read-only dashboard", resp.StatusCode)
	}
}

func TestToggleRequiresTheAdminToken(t *testing.T) {
	ts, _ := serve(t, "admin-token-at-least-16")
	resp, err := ts.Client().PostForm(ts.URL+"/flags/new-checkout/toggle",
		map[string][]string{"token": {"wrong"}, "version": {"1"}})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("got %d, want 403 with a bad token", resp.StatusCode)
	}
}

func TestToggleFlipsTheFlagAndRecordsAReason(t *testing.T) {
	const token = "admin-token-at-least-16"
	ts, st := serve(t, token)

	resp, err := ts.Client().PostForm(ts.URL+"/flags/new-checkout/toggle",
		map[string][]string{"token": {token}, "version": {"1"}, "comment": {"incident 42"}})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	f, err := st.Get(context.Background(), "new-checkout")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !f.Enabled {
		t.Error("the flag was not enabled")
	}

	changes, _ := st.History(context.Background(), "new-checkout", 10)
	if len(changes) == 0 || changes[0].Comment != "incident 42" {
		t.Errorf("the reason was not recorded: %+v", changes)
	}
	// A kill-switch flip should be obvious in the log.
	if changes[0].Action != "enable" {
		t.Errorf("action = %q, want enable", changes[0].Action)
	}
}

func TestToggleRejectsAStaleVersion(t *testing.T) {
	const token = "admin-token-at-least-16"
	ts, _ := serve(t, token)

	// Version 1 is current; claiming 99 means the page was out of date.
	resp, err := ts.Client().PostForm(ts.URL+"/flags/new-checkout/toggle",
		map[string][]string{"token": {token}, "version": {"99"}})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Errorf("got %d, want 412 for a stale version", resp.StatusCode)
	}
}

func TestPagesSetSecurityHeaders(t *testing.T) {
	ts, _ := serve(t, "")
	resp, err := ts.Client().Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	// The dashboard ships no scripts, so the policy can be this tight.
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("Content-Security-Policy = %q", csp)
	}
}
