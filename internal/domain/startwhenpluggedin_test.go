package domain

import (
	"testing"
	"time"
)

// StartWhenPluggedIn is the one case where this controller commands a car that is not already
// charging. It exists because the default behaviour strands a real situation: you plug in, the car
// does not start itself, the sun comes out, and nothing happens all afternoon because the
// controller will only ever resume a session it stopped.
//
// It is safe here and nowhere else — telemetry has just reported the car plugged in, so it is
// awake, and the worry about waking a sleeping vehicle does not apply. Every test below is about
// the gates that must still hold.

func startOptions(enabled bool) ChargingOptions {
	o := DefaultChargingOptions()
	o.StartWhenPluggedIn = enabled
	return o
}

// pluggedIdle is the state that motivated the feature: connector in, car not charging, and no
// marker from us because a person stopped it.
func pluggedIdle(opts ...vehOpt) *VehicleState {
	v := &VehicleState{
		VIN:                 testVIN,
		ChargingState:       StateStopped,
		BatteryLevelPercent: intPtr(63),
		LastUpdated:         testNow.Add(-2 * time.Minute),
	}
	for _, o := range opts {
		o(v)
	}
	return v
}

func TestDoesNotStartAnIdleCarByDefault(t *testing.T) {
	d := Decide(pluggedIdle(), amps(6), true, startOptions(false), testNow)

	if d.Action != ActionSkipNotCharging {
		t.Errorf("Action = %v, want SkipNotCharging — starting must be opt-in", d.Action)
	}
}

func TestStartsAnIdlePluggedInCarWhenEnabled(t *testing.T) {
	d := Decide(pluggedIdle(), amps(6), true, startOptions(true), testNow)

	if d.Action != ActionStartCharging {
		t.Fatalf("Action = %v, want StartCharging", d.Action)
	}
	if !d.ShouldStart() {
		t.Error("expected ShouldStart")
	}
	if d.TargetAmps == nil || *d.TargetAmps != 6 {
		t.Errorf("TargetAmps = %v, want 6", d.TargetAmps)
	}
}

func TestNeverStartsAnUnpluggedCar(t *testing.T) {
	// Disconnected is the whole point of the guard: the car may be miles away and asleep.
	d := Decide(pluggedIdle(vehState(StateDisconnected)), amps(12), true, startOptions(true), testNow)

	if d.Action != ActionSkipNotCharging {
		t.Errorf("Action = %v, want SkipNotCharging for a disconnected car", d.Action)
	}
}

func TestNeverStartsWhileAnOverrideIsActive(t *testing.T) {
	d := Decide(pluggedIdle(vehOverride()), amps(12), true, startOptions(true), testNow)

	if d.Action != ActionSkipOverrideActive {
		t.Errorf("Action = %v, want SkipOverrideActive", d.Action)
	}
}

func TestNeverStartsAboveTheSocCap(t *testing.T) {
	v := pluggedIdle()
	v.BatteryLevelPercent = intPtr(85)

	d := Decide(v, amps(12), true, startOptions(true), testNow)

	if d.Action != ActionSkipAtSocCap {
		t.Errorf("Action = %v, want SkipAtSocCap", d.Action)
	}
}

func TestNeverStartsBelowTheConnectorMinimum(t *testing.T) {
	// 3A of production cannot sustain a 5A session, so starting one would charge from the grid.
	d := Decide(pluggedIdle(), amps(3), true, startOptions(true), testNow)

	if d.Action != ActionSkipInsufficientSolar {
		t.Errorf("Action = %v, want SkipInsufficientSolar", d.Action)
	}
}

func TestNeverStartsWithTheSunDown(t *testing.T) {
	d := Decide(pluggedIdle(), nil, false, startOptions(true), testNow)

	if d.Action != ActionSkipInsufficientSolar {
		t.Errorf("Action = %v, want SkipInsufficientSolar outside the solar window", d.Action)
	}
}

func TestNeverStartsOnStaleTelemetry(t *testing.T) {
	// If the last report is hours old the car is probably asleep, and "plugged in" is a memory
	// rather than an observation.
	v := pluggedIdle()
	v.LastUpdated = testNow.Add(-9 * time.Hour)

	d := Decide(v, amps(12), true, startOptions(true), testNow)

	if d.Action != ActionSkipNoVehicleState {
		t.Errorf("Action = %v, want SkipNoVehicleState", d.Action)
	}
}

func TestAResumeStillWinsOverAStart(t *testing.T) {
	// When we stopped the session ourselves the marker is set, and that path reports ResumeCharging
	// so the log distinguishes "picking up where we left off" from "starting something new".
	v := pluggedIdle()
	stopped := testNow.Add(-time.Hour)
	v.LowSolarStopIssuedAt = &stopped

	d := Decide(v, amps(12), true, startOptions(true), testNow)

	if d.Action != ActionResumeCharging {
		t.Errorf("Action = %v, want ResumeCharging", d.Action)
	}
}

func TestStartsFromEveryPluggedInState(t *testing.T) {
	// Complete, NoPower and Starting all mean the cable is in. Only Disconnected does not.
	for _, state := range []ChargingState{StateStopped, StateComplete, StateNoPower, StateStarting} {
		t.Run(state.String(), func(t *testing.T) {
			d := Decide(pluggedIdle(vehState(state)), amps(9), true, startOptions(true), testNow)
			if d.Action != ActionStartCharging {
				t.Errorf("Action = %v, want StartCharging", d.Action)
			}
		})
	}
}
