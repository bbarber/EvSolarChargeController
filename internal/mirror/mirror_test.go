package mirror

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bbarber/EvSolarChargeController/internal/domain"
	"github.com/bbarber/EvSolarChargeController/internal/store"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestShipsInOrderAndDeletesOnlyOnSuccess(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer service-key" {
			t.Errorf("missing service key auth")
		}
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.URL.Path+"?"+r.URL.RawQuery+" "+string(body))
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	st := newStore(t)
	m := New(Config{URL: srv.URL, ServiceKey: "service-key"}, st, quiet())
	ctx := context.Background()

	m.RecordSolar(ctx, time.Date(2026, 8, 18, 15, 0, 0, 0, time.UTC), 2400, 10)
	m.RecordEvent(ctx, time.Date(2026, 8, 18, 15, 0, 1, 0, time.UTC), "VIN1", "decision", "SetAmps", "sun")

	if err := m.drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("shipped %d rows, want 2: %v", len(got), got)
	}
	if got[0][:36] != "/rest/v1/solar_readings?on_conflict=" {
		t.Errorf("first shipment = %q, want the solar upsert first (order preserved)", got[0])
	}
	pending, _ := st.PendingMirror(ctx, 10)
	if len(pending) != 0 {
		t.Errorf("outbox still holds %d rows after success", len(pending))
	}
}

// A Supabase outage must retain rows and retry — freshness is the only casualty.
func TestAFailedShipmentRetainsTheRowAndBlocksLaterOnes(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	st := newStore(t)
	m := New(Config{URL: srv.URL, ServiceKey: "k"}, st, quiet())
	ctx := context.Background()

	m.RecordSolar(ctx, time.Now(), 2400, 10)
	m.RecordEvent(ctx, time.Now(), "VIN1", "decision", "SetAmps", "sun")

	if err := m.drain(ctx); err == nil {
		t.Fatal("expected the drain to report the failure")
	}
	if calls.Load() != 1 {
		t.Errorf("made %d calls, want 1 — the batch must stop at the first failure to keep order", calls.Load())
	}
	pending, _ := st.PendingMirror(ctx, 10)
	if len(pending) != 2 {
		t.Errorf("outbox holds %d rows, want both retained", len(pending))
	}
}

func TestStatusPayloadCarriesTheSessionAndGates(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	st := newStore(t)
	m := New(Config{URL: srv.URL, ServiceKey: "k"}, st, quiet())
	ctx := context.Background()

	home, online := true, false
	since := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	v := domain.NewVehicleState("VIN1", time.Now())
	v.Session = domain.SessionStoppedForSun
	v.SessionSince = &since
	v.AtHome = &home
	v.Online = &online

	m.RecordStatus(ctx, v)
	if err := m.drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if body["session"] != "StoppedForSun" || body["at_home"] != true || body["online"] != false {
		t.Errorf("payload mismatch: %v", body)
	}
}

// A nil mirror must be safe everywhere — mirroring is optional.
func TestNilMirrorIsANoOp(t *testing.T) {
	var m *Mirror
	m.RecordStatus(context.Background(), domain.NewVehicleState("V", time.Now()))
	m.RecordSolar(context.Background(), time.Now(), 1, 1)
	m.RecordEvent(context.Background(), time.Now(), "V", "decision", "a", "r")
}
