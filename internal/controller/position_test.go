package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/bbarber/EvSolarChargeController/internal/domain"
	"github.com/bbarber/EvSolarChargeController/internal/store"
)

// This is the failure that happened in production on 2026-08-19.
//
// The car drove home through a cellular dead zone, so the last Location frame it managed to send
// placed it away from home. Location transmits on change and a parked car's position never
// changes, so nothing ever corrected it: the car sat on the home connector charging from the grid
// while every evaluation logged SkipNotAtHome. The stale answer has to expire, and the only way
// out of "away" is to ask the car directly.

const homeLat, homeLon = 12.0, 34.0

func homeController(t *testing.T, solar SolarReader, cmd Commander) (*Controller, *store.Store) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "position.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	window, err := domain.NewPollingWindow(domain.DefaultPollingWindowOptions())
	if err != nil {
		t.Fatalf("NewPollingWindow: %v", err)
	}

	opts := domain.DefaultChargingOptions()
	opts.HomeLatitude, opts.HomeLongitude, opts.HomeRadiusM = homeLat, homeLon, 250

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New([]string{testVIN}, st, solar, cmd, window, opts, log), st
}

// strandedVehicle is charging at home, online, with a stale "away" position.
func strandedVehicle(t *testing.T, st *store.Store, at time.Time) {
	t.Helper()
	v := domain.NewVehicleState(testVIN, at)
	v.ChargingState = domain.StateCharging
	v.ChargeAmps = ptrInt(16)
	v.BatteryLevelPercent = ptrInt(50)

	online := true
	v.Online, v.OnlineAt = &online, &at

	away := false
	stale := at.Add(-5 * time.Hour)
	v.AtHome, v.AtHomeAt = &away, &stale

	if err := st.SaveVehicleState(context.Background(), v); err != nil {
		t.Fatalf("SaveVehicleState: %v", err)
	}
}

func ptrInt(v int) *int { return &v }

// lowSolar puts production well under the connector minimum in the decision window, so the correct
// action for a managed session is to stop rather than draw the shortfall from the grid.
func lowSolar(t *testing.T, st *store.Store, at time.Time) {
	t.Helper()
	if err := st.AddSolarReading(context.Background(), at.Add(-5*time.Minute), 300, 1.25); err != nil {
		t.Fatalf("AddSolarReading: %v", err)
	}
}

func TestAStalePositionIsResolvedAndTheSessionIsManagedAgain(t *testing.T) {
	// Solar well under the connector minimum: once the gate opens, the correct action is to stop
	// rather than keep charging from the grid.
	cmd := &fakeCommander{locationLat: 12.0002, locationLon: 34.0002} // ~30m from home
	c, st := homeController(t, &fakeSolar{result: watts(300, testNow)}, cmd)
	strandedVehicle(t, st, testNow)
	ctx := context.Background()
	lowSolar(t, st, testNow)

	if err := c.evaluate(ctx, testNow); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if len(cmd.locationCalls) != 1 {
		t.Fatalf("expected exactly one position read, got %d", len(cmd.locationCalls))
	}
	if cmd.stops != 1 {
		t.Errorf("expected the session to be stopped once the car was known to be home, got %d stops", cmd.stops)
	}

	got, err := st.GetVehicleState(ctx, testVIN)
	if err != nil {
		t.Fatalf("GetVehicleState: %v", err)
	}
	if got.AtHome == nil || !*got.AtHome {
		t.Error("expected the resolved position to be stored as at home")
	}
	if got.AtHomeAt == nil || !got.AtHomeAt.Equal(testNow) {
		t.Errorf("AtHomeAt = %v, want the time of the read", got.AtHomeAt)
	}
}

// A car that really is somewhere else stays untouched — the gate still does its job.
func TestAResolvedAwayPositionLeavesTheSessionAlone(t *testing.T) {
	cmd := &fakeCommander{locationLat: 12.45, locationLon: 34.0} // ~50km away
	c, st := homeController(t, &fakeSolar{result: watts(300, testNow)}, cmd)
	strandedVehicle(t, st, testNow)
	lowSolar(t, st, testNow)

	if err := c.evaluate(context.Background(), testNow); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if cmd.stops != 0 || len(cmd.setAmps) != 0 {
		t.Errorf("expected no commands for a car away from home; stops=%d sets=%v", cmd.stops, cmd.setAmps)
	}
}

// A failed read must not be fatal, and must not strand the loop: the car dozing off between the
// connectivity event and the call returns HTTP 408, which is ordinary.
func TestAFailedPositionReadIsNotFatal(t *testing.T) {
	cmd := &fakeCommander{locationErr: errors.New("HTTP 408: vehicle did not respond")}
	c, st := homeController(t, &fakeSolar{result: watts(300, testNow)}, cmd)
	strandedVehicle(t, st, testNow)
	lowSolar(t, st, testNow)

	if err := c.evaluate(context.Background(), testNow); err != nil {
		t.Fatalf("evaluate returned an error for a failed position read: %v", err)
	}
	if cmd.stops != 0 {
		t.Errorf("expected no command while the position is still unknown, got %d stops", cmd.stops)
	}
}

// The cooldown is what keeps this from becoming the polling loop the design forbids.
func TestPositionIsNotReadOnEveryEvaluation(t *testing.T) {
	cmd := &fakeCommander{locationErr: errors.New("HTTP 408: vehicle did not respond")}
	c, st := homeController(t, &fakeSolar{result: watts(300, testNow)}, cmd)
	strandedVehicle(t, st, testNow)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := c.evaluate(ctx, testNow.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("evaluate: %v", err)
		}
	}
	if len(cmd.locationCalls) != 1 {
		t.Errorf("expected one read across five evaluations inside the cooldown, got %d",
			len(cmd.locationCalls))
	}

	// Past the cooldown, it is willing to ask again.
	if err := c.evaluate(ctx, testNow.Add(11*time.Minute)); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(cmd.locationCalls) != 2 {
		t.Errorf("expected a second read after the cooldown, got %d", len(cmd.locationCalls))
	}
}

// A fresh position is never re-read: the resolve path must stay dormant in the normal case.
func TestNoReadHappensWhenThePositionIsCurrent(t *testing.T) {
	cmd := &fakeCommander{locationLat: homeLat, locationLon: homeLon}
	c, st := homeController(t, &fakeSolar{result: watts(300, testNow)}, cmd)

	v := domain.NewVehicleState(testVIN, testNow)
	v.ChargingState = domain.StateCharging
	v.ChargeAmps = ptrInt(16)
	v.BatteryLevelPercent = ptrInt(50)
	online, home := true, true
	fresh := testNow.Add(-2 * time.Minute)
	v.Online, v.OnlineAt = &online, &testNow
	v.AtHome, v.AtHomeAt = &home, &fresh
	if err := st.SaveVehicleState(context.Background(), v); err != nil {
		t.Fatalf("SaveVehicleState: %v", err)
	}

	if err := c.evaluate(context.Background(), testNow); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(cmd.locationCalls) != 0 {
		t.Errorf("expected no position read for a current position, got %d", len(cmd.locationCalls))
	}
}
