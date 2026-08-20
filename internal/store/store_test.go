package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bbarber/EvSolarChargeController/internal/domain"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var now = time.Date(2026, 8, 11, 14, 0, 0, 0, time.UTC)

func TestVehicleStateRoundTrips(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	since := now.Add(-5 * time.Minute)
	setAt := now.Add(-2 * time.Minute)

	want := &domain.VehicleState{
		VIN:                 "5YJ3E1EA3KF428848",
		ChargeAmps:          ptr(12),
		ReportedMaxAmps:     ptr(16),
		BatteryLevelPercent: ptr(64),
		ChargingState:       domain.StateCharging,
		Session:             domain.SessionOverridden,
		SessionSince:        &since,
		LastSetAmps:         ptr(11),
		LastSetAt:           &setAt,
		LastUpdated:         now,
	}

	if err := s.SaveVehicleState(ctx, want); err != nil {
		t.Fatalf("SaveVehicleState: %v", err)
	}

	got, err := s.GetVehicleState(ctx, want.VIN)
	if err != nil {
		t.Fatalf("GetVehicleState: %v", err)
	}

	if got.VIN != want.VIN || got.ChargingState != domain.StateCharging || got.Session != domain.SessionOverridden {
		t.Errorf("scalar fields did not round trip: %+v", got)
	}
	if *got.ChargeAmps != 12 || *got.ReportedMaxAmps != 16 || *got.BatteryLevelPercent != 64 || *got.LastSetAmps != 11 {
		t.Errorf("nullable ints did not round trip: %+v", got)
	}
	if !got.LastUpdated.Equal(now) || !got.SessionSince.Equal(since) || !got.LastSetAt.Equal(setAt) {
		t.Errorf("timestamps did not round trip: %+v", got)
	}
}

func TestGetVehicleStateReturnsNilForAnUnknownVin(t *testing.T) {
	got, err := newStore(t).GetVehicleState(context.Background(), "nope")
	if err != nil {
		t.Fatalf("GetVehicleState: %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil for an unknown VIN", got)
	}
}

func TestSaveVehicleStateUpsertsRatherThanDuplicating(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	v := domain.NewVehicleState("VIN1", now)
	if err := s.SaveVehicleState(ctx, v); err != nil {
		t.Fatalf("first save: %v", err)
	}

	v.ChargingState = domain.StateComplete
	v.LastUpdated = now.Add(time.Minute)
	if err := s.SaveVehicleState(ctx, v); err != nil {
		t.Fatalf("second save: %v", err)
	}

	got, err := s.GetVehicleState(ctx, "VIN1")
	if err != nil {
		t.Fatalf("GetVehicleState: %v", err)
	}
	if got.ChargingState != domain.StateComplete {
		t.Errorf("ChargingState = %v, want Complete", got.ChargingState)
	}
}

func TestMaxAmpsSinceTakesTheWindowMaximum(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// A dip inside the window must not lower the result: taking the maximum is what stops a
	// passing cloud from cutting the charge current.
	readings := []struct {
		at   time.Time
		amps float64
	}{
		{now.Add(-90 * time.Minute), 15}, // outside the window
		{now.Add(-50 * time.Minute), 11},
		{now.Add(-30 * time.Minute), 2},
		{now.Add(-10 * time.Minute), 9},
	}
	for _, r := range readings {
		if err := s.AddSolarReading(ctx, r.at, r.amps*240, r.amps, nil); err != nil {
			t.Fatalf("AddSolarReading: %v", err)
		}
	}

	got, err := s.MaxAmpsSince(ctx, now.Add(-60*time.Minute))
	if err != nil {
		t.Fatalf("MaxAmpsSince: %v", err)
	}
	if got == nil || *got != 11 {
		t.Errorf("MaxAmpsSince = %v, want 11 (the 15 is outside the window)", got)
	}
}

func TestMaxAmpsSinceIsNilWhenTheWindowIsEmpty(t *testing.T) {
	// Nil is not zero. A failed poll leaves the window empty, which is not evidence of low
	// production and must not stop a running session.
	s := newStore(t)
	ctx := context.Background()

	if err := s.AddSolarReading(ctx, now.Add(-3*time.Hour), 2400, 10, nil); err != nil {
		t.Fatalf("AddSolarReading: %v", err)
	}

	got, err := s.MaxAmpsSince(ctx, now.Add(-60*time.Minute))
	if err != nil {
		t.Fatalf("MaxAmpsSince: %v", err)
	}
	if got != nil {
		t.Errorf("MaxAmpsSince = %v, want nil", got)
	}
}

func TestPruneSolarReadingsDropsOnlyOldRows(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	for _, at := range []time.Time{now.Add(-3 * time.Hour), now.Add(-2 * time.Hour), now.Add(-10 * time.Minute)} {
		if err := s.AddSolarReading(ctx, at, 2400, 10, nil); err != nil {
			t.Fatalf("AddSolarReading: %v", err)
		}
	}

	deleted, err := s.PruneSolarReadings(ctx, now.Add(-60*time.Minute))
	if err != nil {
		t.Fatalf("PruneSolarReadings: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted %d rows, want 2", deleted)
	}

	remaining, err := s.MaxAmpsSince(ctx, time.Time{})
	if err != nil {
		t.Fatalf("MaxAmpsSince: %v", err)
	}
	if remaining == nil {
		t.Error("expected the recent reading to survive")
	}
}

func TestApiUsageCountsPerProviderAndMonth(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := s.RecordAPICall(ctx, "enphase", now, false); err != nil {
			t.Fatalf("RecordAPICall: %v", err)
		}
	}
	count, err := s.RecordAPICall(ctx, "enphase", now, true)
	if err != nil {
		t.Fatalf("RecordAPICall: %v", err)
	}
	if count != 4 {
		t.Errorf("count = %d, want 4", count)
	}

	// A different provider and a different month must not share the counter — the Enphase budget
	// is per-month and the plan resets on the first.
	if _, err := s.RecordAPICall(ctx, "tesla", now, false); err != nil {
		t.Fatalf("RecordAPICall: %v", err)
	}
	if got, _ := s.MonthlyCallCount(ctx, "enphase", now); got != 4 {
		t.Errorf("enphase count = %d, want 4", got)
	}
	if got, _ := s.MonthlyCallCount(ctx, "enphase", now.AddDate(0, 1, 0)); got != 0 {
		t.Errorf("next month count = %d, want 0", got)
	}
}

func TestSecretsRoundTripAndOverwrite(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if _, err := s.GetSecret(ctx, "enphase-refresh-token"); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("expected ErrSecretNotFound, got %v", err)
	}

	if err := s.PutSecret(ctx, "enphase-refresh-token", "first"); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	// Refresh tokens rotate on use, so overwriting in place is the normal path.
	if err := s.PutSecret(ctx, "enphase-refresh-token", "rotated"); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}

	got, err := s.GetSecret(ctx, "enphase-refresh-token")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if got != "rotated" {
		t.Errorf("GetSecret = %q, want %q", got, "rotated")
	}
}

func TestDatabaseFileIsNotWorldReadable(t *testing.T) {
	// It holds live refresh tokens.
	path := filepath.Join(t.TempDir(), "perms.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	info, err := statFile(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

func ptr(v int) *int { return &v }

func statFile(path string) (os.FileInfo, error) { return os.Stat(path) }

// Opening a database created by the ORIGINAL schema must migrate it to a writable state. The
// legacy override_active column carried NOT NULL, and a save that no longer supplies it failed on
// every frame — in production only, because fresh test databases never had the column. This test
// builds the old schema by hand so that gap can never reopen.
func TestOpensAndMigratesAnOriginalSchemaDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("opening raw: %v", err)
	}
	if _, err := raw.Exec(`
        CREATE TABLE vehicle_state (
            vin                      TEXT PRIMARY KEY,
            charge_amps              INTEGER,
            reported_max_amps        INTEGER,
            battery_level_percent    INTEGER,
            soc_stop_issued_at       TEXT,
            low_solar_stop_issued_at TEXT,
            charging_state           TEXT    NOT NULL,
            override_active          INTEGER NOT NULL,
            override_detected_at     TEXT,
            last_set_amps            INTEGER,
            last_set_at              TEXT,
            last_updated             TEXT    NOT NULL
        );
        INSERT INTO vehicle_state (vin, charging_state, override_active, last_updated, override_detected_at)
        VALUES ('LEGACYVIN', 'Charging', 1, '2026-08-16T12:00:00Z', '2026-08-16T11:00:00Z');`); err != nil {
		t.Fatalf("building legacy schema: %v", err)
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a legacy database: %v", err)
	}
	defer s.Close()

	ctx := context.Background()

	// The marker must have been folded into the session before the columns were dropped.
	got, err := s.GetVehicleState(ctx, "LEGACYVIN")
	if err != nil {
		t.Fatalf("GetVehicleState: %v", err)
	}
	if got.Session != domain.SessionOverridden {
		t.Errorf("Session = %v, want Overridden folded from the legacy marker", got.Session)
	}

	// And — the actual production failure — saving must work.
	got.ChargeAmps = ptr(22)
	if err := s.SaveVehicleState(ctx, got); err != nil {
		t.Fatalf("SaveVehicleState on a migrated database: %v", err)
	}
}
