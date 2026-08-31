package controller

import (
	"context"
	"testing"
	"time"

	"github.com/teslamotors/fleet-telemetry/protos"

	"github.com/bbarber/EvSolarChargeController/internal/domain"
)

// The pause switch is the one control a person reaches for when the system is doing something they
// do not want. It has to hold against every command path, including ones added later — so this
// suite drives the controller through each of them rather than testing the gate in isolation.

type fakePauser struct {
	until *time.Time
}

func (f *fakePauser) Paused(now time.Time) bool {
	return f.until != nil && now.Before(*f.until)
}
func (f *fakePauser) Until() *time.Time { return f.until }

func pausedUntil(t time.Time) *fakePauser { return &fakePauser{until: &t} }

// A car charging in full sun, which would otherwise have its current set.
func chargingInSun(t *testing.T, st interface {
	AddSolarReading(ctx context.Context, at time.Time, watts, amps float64, houseWatts *float64) error
}, at time.Time) {
	t.Helper()
	if err := st.AddSolarReading(context.Background(), at.Add(-5*time.Minute), 2400, 10, nil); err != nil {
		t.Fatalf("AddSolarReading: %v", err)
	}
}

func TestNoAmpChangeIsSentWhilePaused(t *testing.T) {
	cmd := &fakeCommander{}
	c, st := newController(t, &fakeSolar{}, cmd)
	c.SetPauser(pausedUntil(testNow.Add(6 * time.Hour)))
	chargingVehicle(t, st, testNow)
	chargingInSun(t, st, testNow)

	if err := c.evaluate(context.Background(), testNow); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(cmd.setAmps) != 0 {
		t.Errorf("expected no amp change while paused, got %v", cmd.setAmps)
	}
}

func TestNoStopIsSentWhilePaused(t *testing.T) {
	// Sun below the connector minimum: unpaused, this stops the session.
	cmd := &fakeCommander{}
	c, st := newController(t, &fakeSolar{}, cmd)
	c.SetPauser(pausedUntil(testNow.Add(6 * time.Hour)))
	chargingVehicle(t, st, testNow)
	if err := st.AddSolarReading(context.Background(), testNow.Add(-5*time.Minute), 300, 1.25, nil); err != nil {
		t.Fatalf("AddSolarReading: %v", err)
	}

	if err := c.evaluate(context.Background(), testNow); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if cmd.stops != 0 {
		t.Errorf("expected no stop while paused, got %d", cmd.stops)
	}
}

func TestNoWakeIsSentWhilePaused(t *testing.T) {
	cmd := &fakeCommander{}
	c, st := newController(t, &fakeSolar{}, cmd)
	c.opts.WakeToCharge = true
	c.SetPauser(pausedUntil(testNow.Add(6 * time.Hour)))

	// Asleep, plugged in, with sun sustained — every wake gate would otherwise pass.
	v := domain.NewVehicleState(testVIN, testNow)
	v.ChargingState = domain.StateStopped
	v.BatteryLevelPercent = ptrInt(40)
	offline := false
	seen := testNow.Add(-30 * time.Minute)
	v.Online, v.OnlineAt = &offline, &seen
	if err := st.SaveVehicleState(context.Background(), v); err != nil {
		t.Fatalf("SaveVehicleState: %v", err)
	}
	for _, ago := range []time.Duration{40, 20} {
		if err := st.AddSolarReading(context.Background(), testNow.Add(-ago*time.Minute), 2400, 10, nil); err != nil {
			t.Fatalf("AddSolarReading: %v", err)
		}
	}

	if err := c.evaluate(context.Background(), testNow); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(cmd.wakes) != 0 {
		t.Errorf("expected no wake while paused, got %v", cmd.wakes)
	}
}

// The pause is an instant, not a flag, so it lapses on its own. Nothing runs at midnight to clear
// it, and a controller that was down over the boundary comes back willing to charge.
func TestCommandsResumeOnceThePauseLapses(t *testing.T) {
	cmd := &fakeCommander{}
	c, st := newController(t, &fakeSolar{}, cmd)
	lapsed := testNow.Add(-time.Minute)
	c.SetPauser(&fakePauser{until: &lapsed})
	chargingVehicle(t, st, testNow)
	chargingInSun(t, st, testNow)

	if err := c.evaluate(context.Background(), testNow); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(cmd.setAmps) == 0 {
		t.Error("expected commands to resume once the pause expired")
	}
}

// Without a pauser the controller must behave exactly as it did before this existed.
func TestNoPauserMeansNothingIsWithheld(t *testing.T) {
	cmd := &fakeCommander{}
	c, st := newController(t, &fakeSolar{}, cmd)
	chargingVehicle(t, st, testNow)
	chargingInSun(t, st, testNow)

	if err := c.evaluate(context.Background(), testNow); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(cmd.setAmps) == 0 {
		t.Error("expected a command with no pauser installed")
	}
}

// Telemetry must keep flowing while paused: the pause withholds commands, it does not blind the
// system. A car that unplugs while paused still has to be seen to unplug.
func TestTelemetryIsStillFoldedWhilePaused(t *testing.T) {
	cmd := &fakeCommander{}
	c, st := newController(t, &fakeSolar{}, cmd)
	c.SetPauser(pausedUntil(testNow.Add(6 * time.Hour)))
	chargingVehicle(t, st, testNow)

	frame := telemetryFrame(t, testNow, &protos.Datum{
		Key: protos.Field_DetailedChargeState,
		Value: &protos.Value{Value: &protos.Value_DetailedChargeStateValue{
			DetailedChargeStateValue: protos.DetailedChargeStateValue_DetailedChargeStateDisconnected,
		}},
	})
	if err := c.HandleTelemetry(context.Background(), frame); err != nil {
		t.Fatalf("HandleTelemetry: %v", err)
	}

	got, err := st.GetVehicleState(context.Background(), testVIN)
	if err != nil {
		t.Fatalf("GetVehicleState: %v", err)
	}
	if got.ChargingState != domain.StateDisconnected {
		t.Errorf("ChargingState = %v, want Disconnected: telemetry must still be recorded while paused", got.ChargingState)
	}
}
