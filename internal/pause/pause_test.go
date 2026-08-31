package pause

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

var now = time.Date(2026, 8, 31, 20, 0, 0, 0, time.UTC)

func newSwitch(t *testing.T, handler http.HandlerFunc) (*Switch, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	s := New(srv.URL, "service-key", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if s == nil {
		t.Fatal("expected a switch")
	}
	return s, srv
}

func TestReadsAPauseInstant(t *testing.T) {
	s, _ := newSwitch(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer service-key" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("apikey"); got != "service-key" {
			t.Errorf("apikey = %q", got)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %s; the VM must only ever read this", r.Method)
		}
		w.Write([]byte(`[{"paused_until":"2026-09-01T05:00:00+00:00"}]`))
	})

	s.refresh(context.Background())

	if !s.Paused(now) {
		t.Error("expected paused before the instant")
	}
	if s.Paused(time.Date(2026, 9, 1, 6, 0, 0, 0, time.UTC)) {
		t.Error("expected not paused after the instant")
	}
}

func TestNullAndEmptyMeanNotPaused(t *testing.T) {
	for name, body := range map[string]string{
		"null column": `[{"paused_until":null}]`,
		"no row":      `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := newSwitch(t, func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(body))
			})
			s.refresh(context.Background())
			if s.Paused(now) {
				t.Error("expected not paused")
			}
		})
	}
}

// Charging must never depend on Supabase being reachable. An outage cannot pause the system...
func TestAFailedReadDoesNotPause(t *testing.T) {
	s, _ := newSwitch(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	s.refresh(context.Background())
	if s.Paused(now) {
		t.Error("a failed read must not pause the system")
	}
}

// ...and equally, an outage must not silently un-pause it.
func TestAFailedReadKeepsTheLastKnownPause(t *testing.T) {
	fail := false
	s, _ := newSwitch(t, func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`[{"paused_until":"2026-09-01T05:00:00+00:00"}]`))
	})

	s.refresh(context.Background())
	fail = true
	s.refresh(context.Background())

	if !s.Paused(now) {
		t.Error("expected the last known pause to stand through an outage")
	}
}

func TestNoSupabaseMeansNoSwitch(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if New("", "key", log) != nil || New("https://x", "", log) != nil {
		t.Error("expected nil without both a URL and a key")
	}
	// A nil switch must be safe to call: that is the un-configured deployment.
	var s *Switch
	if s.Paused(now) || s.Until() != nil {
		t.Error("a nil switch must never pause")
	}
	s.Run(context.Background())
}

func TestOnlyTalksToTheConfiguredHost(t *testing.T) {
	s, srv := newSwitch(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	})
	if got, want := "http://"+s.endpointHost(), srv.URL; got != want {
		t.Errorf("endpoint host = %s, want %s", got, want)
	}
}
