package controller

import (
	"context"
	"testing"
	"time"

	"github.com/bbarber/EvSolarChargeController/internal/domain"
)

// Reproduces the failure observed live on 2026-08-16.
//
// Three consecutive ticks had enough sun — 5.45A, 6.42A, 5.75A — on a Sunday, with the car asleep
// and plugged in at 63%. Every human-facing condition for a wake was met and the wake never fired.
//
// The cause was one value answering three questions. Readings were pruned at the lookback horizon,
// the wake gate counted readings over that same horizon, and polling runs at exactly that interval
// — so the previous reading was deleted moments before the gate counted it. The table held one
// reading; the gate needed two; it could never pass.
//
// This test drives real ticks through the real store so the pruning actually happens.
//
// The reading timestamps matter and are not the tick times. Enphase reports its own last_report_at,
// observed in production about two minutes before the tick that fetched it — and that offset is
// exactly what pushed the previous reading past a cutoff set at the tick minus the lookback. A
// version of this test using tick times for readings passes against the broken code, because the
// previous reading lands precisely on the boundary and survives.

func TestSustainedSunAcrossTicksProducesAWake(t *testing.T) {
	// 2026-08-16 is a Sunday. 19:00 UTC is 14:00 America/Chicago, inside the window.
	start := time.Date(2026, 8, 16, 19, 0, 0, 0, time.UTC)

	solar := &fakeSolar{}
	cmd := &fakeCommander{}
	c, st := newController(t, solar, cmd)

	opts := domain.DefaultChargingOptions()
	opts.WakeToCharge = true
	c.SetChargingOptions(opts)

	ctx := context.Background()

	// The car is asleep and plugged in, as it was in the field.
	offline := false
	seen := start.Add(-30 * time.Minute)
	v := domain.NewVehicleState(testVIN, seen)
	v.ChargingState = domain.StateStopped
	v.BatteryLevelPercent = intPtr(63)
	v.Online, v.OnlineAt = &offline, &seen
	if err := st.SaveVehicleState(ctx, v); err != nil {
		t.Fatalf("SaveVehicleState: %v", err)
	}

	// Three ticks, twenty minutes apart, each with sun over the 5A floor — the real figures.
	ticks := []struct {
		at    time.Time
		watts float64
	}{
		{start, 1307},                       // 5.45A
		{start.Add(20 * time.Minute), 1540}, // 6.42A
		{start.Add(40 * time.Minute), 1380}, // 5.75A
	}

	for _, tick := range ticks {
		// Enphase's last_report_at lags the poll; production showed roughly two minutes.
		solar.result = watts(tick.watts, tick.at.Add(-2*time.Minute))
		if err := c.Evaluate(ctx, tick.at); err != nil {
			t.Fatalf("Evaluate at %s: %v", tick.at.Format("15:04"), err)
		}
	}

	// By the third tick the sustained window holds three readings, all above the minimum.
	above, err := st.ReadingsAboveSince(ctx,
		ticks[2].at.Add(-opts.SustainedWindow), float64(opts.MinChargeAmps)-0.5)
	if err != nil {
		t.Fatalf("ReadingsAboveSince: %v", err)
	}
	if above < 2 {
		t.Fatalf("sustained window held %d readings above the minimum, want at least 2 — "+
			"this is the bug: pruning removed them before the gate could count them", above)
	}

	if len(cmd.wakes) != 1 {
		t.Fatalf("wakes = %v, want exactly one after three sustained ticks", cmd.wakes)
	}

	midnight := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	if n, _ := st.WakesSince(ctx, testVIN, midnight); n != 1 {
		t.Errorf("wakes recorded = %d, want 1", n)
	}
}

// A single good reading among poor ones is a sunbreak, not a window worth waking for. The fix
// must not turn the gate into a hair trigger.
func TestASingleGoodReadingDoesNotWake(t *testing.T) {
	start := time.Date(2026, 8, 16, 19, 0, 0, 0, time.UTC)

	solar := &fakeSolar{}
	cmd := &fakeCommander{}
	c, st := newController(t, solar, cmd)

	opts := domain.DefaultChargingOptions()
	opts.WakeToCharge = true
	c.SetChargingOptions(opts)

	ctx := context.Background()
	offline := false
	seen := start.Add(-30 * time.Minute)
	v := domain.NewVehicleState(testVIN, seen)
	v.ChargingState = domain.StateStopped
	v.BatteryLevelPercent = intPtr(63)
	v.Online, v.OnlineAt = &offline, &seen
	if err := st.SaveVehicleState(ctx, v); err != nil {
		t.Fatalf("SaveVehicleState: %v", err)
	}

	// Poor, poor, then one bright reading.
	for i, w := range []float64{575, 600, 1540} {
		at := start.Add(time.Duration(i*20) * time.Minute)
		solar.result = watts(w, at.Add(-2*time.Minute))
		if err := c.Evaluate(ctx, at); err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
	}

	if len(cmd.wakes) != 0 {
		t.Errorf("woke on a single bright reading: %v", cmd.wakes)
	}
}

// Longer retention must not change what the car is asked to draw: the target is a maximum over the
// trailing lookback, and older rows are outside it by definition.
func TestLongerRetentionDoesNotChangeTheTarget(t *testing.T) {
	start := time.Date(2026, 8, 16, 19, 0, 0, 0, time.UTC)

	solar := &fakeSolar{}
	cmd := &fakeCommander{}
	c, st := newController(t, solar, cmd)

	ctx := context.Background()
	chargingVehicle(t, st, start)

	// An hour-old peak that is now retained but must not influence the target.
	if err := st.AddSolarReading(ctx, start.Add(-3*time.Hour), 3840, 16); err != nil {
		t.Fatalf("AddSolarReading: %v", err)
	}

	solar.result = watts(1440, start) // 6A now
	if err := c.Evaluate(ctx, start); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if len(cmd.setAmps) != 1 || cmd.setAmps[0] != 6 {
		t.Errorf("setAmps = %v, want [6] — a retained old peak must not raise the target", cmd.setAmps)
	}
}
