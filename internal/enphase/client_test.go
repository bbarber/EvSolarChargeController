package enphase

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/bbarber/EvSolarChargeController/internal/store"
)

var testNow = time.Date(2026, 8, 11, 14, 0, 0, 0, time.UTC)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newHarness wires the client against the real SQLite store, so budget accounting and refresh
// token rotation are exercised end to end rather than against a mock that cannot drift.
func newHarness(t *testing.T, handler http.HandlerFunc) (*Client, *store.Store, *httptest.Server) {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if err := st.PutSecret(context.Background(), "enphase-refresh-token", "seed-refresh"); err != nil {
		t.Fatalf("seeding refresh token: %v", err)
	}

	opts := DefaultOptions()
	opts.BaseURL = srv.URL + "/api/v4"
	opts.TokenURL = srv.URL + "/oauth/token"
	opts.ClientID = "client"
	opts.ClientSecret = "secret"
	opts.APIKey = "api-key"
	opts.SystemID = "12345"

	return New(opts, st, st, quietLogger()), st, srv
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func TestReturnsCurrentProduction(t *testing.T) {
	reportedAt := testNow.Add(-90 * time.Second)

	c, st, _ := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			writeJSON(w, map[string]any{"access_token": "at", "refresh_token": "seed-refresh", "expires_in": 86400})
		case "/api/v4/systems/12345/summary":
			if got := r.Header.Get("Authorization"); got != "Bearer at" {
				t.Errorf("Authorization = %q, want Bearer at", got)
			}
			// The v4 API wants the key as a query parameter as well as the bearer token.
			if got := r.URL.Query().Get("key"); got != "api-key" {
				t.Errorf("key query = %q, want api-key", got)
			}
			writeJSON(w, map[string]any{"current_power": 3840.0, "last_report_at": reportedAt.Unix()})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	got := c.CurrentProduction(context.Background(), testNow)

	if !got.Success() {
		t.Fatalf("expected success, got %+v", got)
	}
	if got.Production.Watts != 3840 {
		t.Errorf("Watts = %v, want 3840", got.Production.Watts)
	}
	if !got.Production.ReadingAt.Equal(reportedAt.UTC()) {
		t.Errorf("ReadingAt = %v, want %v", got.Production.ReadingAt, reportedAt.UTC())
	}

	// Only the summary call counts; the token call is not billed against the Watt plan.
	if count, _ := st.MonthlyCallCount(context.Background(), Provider, testNow); count != 1 {
		t.Errorf("call count = %d, want 1", count)
	}
}

func TestRefusesToPollOnceTheBudgetIsExhausted(t *testing.T) {
	calls := 0
	c, st, _ := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeJSON(w, map[string]any{"access_token": "at", "expires_in": 86400, "current_power": 100.0})
	})

	ctx := context.Background()
	c.opts.MonthlyCallBudget = 3
	for i := 0; i < 3; i++ {
		if _, err := st.RecordAPICall(ctx, Provider, testNow, false); err != nil {
			t.Fatalf("RecordAPICall: %v", err)
		}
	}

	got := c.CurrentProduction(ctx, testNow)

	if got.Reason != ReasonQuotaGuard {
		t.Errorf("Reason = %q, want quota_guard", got.Reason)
	}
	if calls != 0 {
		t.Errorf("made %d HTTP calls, want 0 — the guard must stop before the wire", calls)
	}
}

func TestPersistsTheRotatedRefreshToken(t *testing.T) {
	// Enphase rotates the refresh token on every use. Missing this locks out the next start.
	c, st, _ := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			if got := r.URL.Query().Get("refresh_token"); got != "seed-refresh" {
				t.Errorf("refresh_token = %q, want seed-refresh", got)
			}
			user, pass, ok := r.BasicAuth()
			if !ok || user != "client" || pass != "secret" {
				t.Errorf("expected basic auth client/secret, got %q/%q ok=%v", user, pass, ok)
			}
			writeJSON(w, map[string]any{"access_token": "at", "refresh_token": "rotated", "expires_in": 86400})
			return
		}
		writeJSON(w, map[string]any{"current_power": 1200.0})
	})

	c.CurrentProduction(context.Background(), testNow)

	got, err := st.GetSecret(context.Background(), "enphase-refresh-token")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if got != "rotated" {
		t.Errorf("stored refresh token = %q, want rotated", got)
	}
}

func TestCachesTheAccessTokenAcrossPolls(t *testing.T) {
	tokenCalls := 0
	c, _, _ := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			tokenCalls++
			writeJSON(w, map[string]any{"access_token": "at", "expires_in": 86400})
			return
		}
		writeJSON(w, map[string]any{"current_power": 1200.0})
	})

	ctx := context.Background()
	c.CurrentProduction(ctx, testNow)
	c.CurrentProduction(ctx, testNow.Add(20*time.Minute))
	c.CurrentProduction(ctx, testNow.Add(40*time.Minute))

	if tokenCalls != 1 {
		t.Errorf("token calls = %d, want 1 — a day-long token should not be refetched per cycle", tokenCalls)
	}
}

func TestRefreshesTheAccessTokenWithinTheSkew(t *testing.T) {
	tokenCalls := 0
	c, _, _ := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			tokenCalls++
			writeJSON(w, map[string]any{"access_token": "at", "expires_in": 3600})
			return
		}
		writeJSON(w, map[string]any{"current_power": 1200.0})
	})

	ctx := context.Background()
	c.CurrentProduction(ctx, testNow)
	// 40 minutes on, the hour-long token is inside the 30-minute skew, so it must be refreshed.
	c.CurrentProduction(ctx, testNow.Add(40*time.Minute))

	if tokenCalls != 2 {
		t.Errorf("token calls = %d, want 2", tokenCalls)
	}
}

func TestRateLimitIsReportedWithoutRetrying(t *testing.T) {
	summaryCalls := 0
	c, _, _ := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			writeJSON(w, map[string]any{"access_token": "at", "expires_in": 86400})
			return
		}
		summaryCalls++
		w.WriteHeader(http.StatusTooManyRequests)
	})

	got := c.CurrentProduction(context.Background(), testNow)

	if got.Reason != ReasonRateLimited {
		t.Errorf("Reason = %q, want rate_limited", got.Reason)
	}
	if summaryCalls != 1 {
		t.Errorf("summary calls = %d, want exactly 1 — retrying would burn the monthly budget", summaryCalls)
	}
}

func TestAFailedCallStillCountsAgainstTheBudget(t *testing.T) {
	c, st, _ := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			writeJSON(w, map[string]any{"access_token": "at", "expires_in": 86400})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})

	got := c.CurrentProduction(context.Background(), testNow)

	if got.Reason != ReasonUnexpected {
		t.Errorf("Reason = %q, want unexpected_response", got.Reason)
	}
	if count, _ := st.MonthlyCallCount(context.Background(), Provider, testNow); count != 1 {
		t.Errorf("call count = %d, want 1 — a 500 still left the process", count)
	}
}

func TestMissingCurrentPowerIsNotTreatedAsZero(t *testing.T) {
	// Zero would look like a total production loss and stop a charging session.
	c, _, _ := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			writeJSON(w, map[string]any{"access_token": "at", "expires_in": 86400})
			return
		}
		writeJSON(w, map[string]any{"status": "normal"})
	})

	got := c.CurrentProduction(context.Background(), testNow)

	if got.Success() {
		t.Error("expected a failure when current_power is absent")
	}
	if got.Reason != ReasonUnexpected {
		t.Errorf("Reason = %q, want unexpected_response", got.Reason)
	}
}

func TestAuthFailureIsReportedWithoutSpendingASummaryCall(t *testing.T) {
	summaryCalls := 0
	c, st, _ := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		summaryCalls++
	})

	got := c.CurrentProduction(context.Background(), testNow)

	if got.Reason != ReasonAuthFailed {
		t.Errorf("Reason = %q, want auth_failed", got.Reason)
	}
	if summaryCalls != 0 {
		t.Errorf("summary calls = %d, want 0", summaryCalls)
	}
	if count, _ := st.MonthlyCallCount(context.Background(), Provider, testNow); count != 0 {
		t.Errorf("call count = %d, want 0 — no summary call was made", count)
	}
}

func TestMissingRefreshTokenFailsClearly(t *testing.T) {
	c, st, _ := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	// Simulate a store that was never seeded.
	c.opts.RefreshTokenSecretName = "never-seeded"
	_ = st

	got := c.CurrentProduction(context.Background(), testNow)

	if got.Reason != ReasonAuthFailed {
		t.Errorf("Reason = %q, want auth_failed", got.Reason)
	}
}
