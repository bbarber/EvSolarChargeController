package domain

import (
	"testing"
	"time"
)

// Resolving a position is the one read this controller makes against the car. Every gate exists so
// it cannot become the routine polling the design forbids, and each is tested separately.

var posNow = time.Date(2026, 8, 19, 14, 0, 0, 0, time.UTC)

func posOptions() ChargingOptions {
	o := DefaultChargingOptions()
	o.HomeLatitude, o.HomeLongitude, o.HomeRadiusM = 12.0, 34.0, 250
	return o
}

// Plugged in, online, and the stored position is old — the case that stranded a real session.
func posReady() *VehicleState {
	online := true
	stale := posNow.Add(-5 * time.Hour)
	away := false
	return &VehicleState{
		VIN:           "VIN1",
		ChargingState: StateCharging,
		Online:        &online,
		AtHome:        &away,
		AtHomeAt:      &stale,
	}
}

func TestResolvesAStalePosition(t *testing.T) {
	d := DecidePositionFix(posReady(), nil, posOptions(), posNow)
	if !d.Resolve {
		t.Errorf("expected a resolve for a 5-hour-old position: %s", d.Reason)
	}
}

func TestResolvesWhenPositionWasNeverKnown(t *testing.T) {
	v := posReady()
	v.AtHome, v.AtHomeAt = nil, nil
	if d := DecidePositionFix(v, nil, posOptions(), posNow); !d.Resolve {
		t.Errorf("expected a resolve when no position has ever been seen: %s", d.Reason)
	}
}

// A position from minutes ago is authoritative; asking again would be the polling loop the
// "never poll the vehicle" rule exists to prevent.
func TestDoesNotResolveAFreshPosition(t *testing.T) {
	v := posReady()
	fresh := posNow.Add(-2 * time.Minute)
	v.AtHomeAt = &fresh
	if d := DecidePositionFix(v, nil, posOptions(), posNow); d.Resolve {
		t.Errorf("expected no resolve for a 2-minute-old position: %s", d.Reason)
	}
}

// The whole point of the gates: never touch a car that might be asleep. A read against a sleeping
// vehicle is exactly the disturbance the invariant protects against.
func TestNeverResolvesForACarThatIsNotOnline(t *testing.T) {
	offline := false
	for name, v := range map[string]*VehicleState{
		"offline": func() *VehicleState { s := posReady(); s.Online = &offline; return s }(),
		"unknown": func() *VehicleState { s := posReady(); s.Online = nil; return s }(),
	} {
		t.Run(name, func(t *testing.T) {
			if d := DecidePositionFix(v, nil, posOptions(), posNow); d.Resolve {
				t.Errorf("expected no resolve: %s", d.Reason)
			}
		})
	}
}

// An unplugged car's position is nobody's business — that is the routine collection the rule bans.
func TestNeverResolvesForAnUnpluggedCar(t *testing.T) {
	v := posReady()
	v.ChargingState = StateDisconnected
	if d := DecidePositionFix(v, nil, posOptions(), posNow); d.Resolve {
		t.Errorf("expected no resolve for an unplugged car: %s", d.Reason)
	}
}

func TestNeverResolvesOnAFastCharger(t *testing.T) {
	v := posReady()
	dc := true
	v.FastCharger = &dc
	if d := DecidePositionFix(v, nil, posOptions(), posNow); d.Resolve {
		t.Errorf("expected no resolve on a DC fast charger: %s", d.Reason)
	}
}

// A car that cannot be resolved must not be asked on every telemetry frame.
func TestRespectsTheRefreshCooldown(t *testing.T) {
	opts := posOptions()
	recent := posNow.Add(-1 * time.Minute)
	if d := DecidePositionFix(posReady(), &recent, opts, posNow); d.Resolve {
		t.Errorf("expected no resolve inside the cooldown: %s", d.Reason)
	}

	old := posNow.Add(-opts.PositionRefreshCooldown - time.Minute)
	if d := DecidePositionFix(posReady(), &old, opts, posNow); !d.Resolve {
		t.Errorf("expected a resolve once the cooldown expired: %s", d.Reason)
	}
}

func TestDoesNotResolveWhenTheHomeGateIsOff(t *testing.T) {
	opts := posOptions()
	opts.HomeLatitude, opts.HomeLongitude = 0, 0
	if d := DecidePositionFix(posReady(), nil, opts, posNow); d.Resolve {
		t.Errorf("expected no resolve with the home gate off: %s", d.Reason)
	}
}

// The fix stores the verdict and its age, never the coordinate.
func TestApplyPositionFixStoresOnlyTheVerdict(t *testing.T) {
	opts := posOptions()
	v := posReady()

	ApplyPositionFix(v, 12.0002, 34.0002, posNow, opts)
	if v.AtHome == nil || !*v.AtHome {
		t.Error("expected a point ~30m away to read as home")
	}
	if v.AtHomeAt == nil || !v.AtHomeAt.Equal(posNow) {
		t.Errorf("AtHomeAt = %v, want %v", v.AtHomeAt, posNow)
	}

	ApplyPositionFix(v, 12.05, 34.0, posNow, opts)
	if v.AtHome == nil || *v.AtHome {
		t.Error("expected a point 5km away to read as away")
	}
}

// The real failure, 2026-08-20: a car drove home, sent its last Location frame 80 seconds before
// joining the home wifi — still short of the radius — then parked and plugged in. Location
// transmits on change, so a parked car sends nothing further and the "away" verdict stands. It was
// minutes old, far inside PositionMaxAge, so the staleness gate never fired and the session went
// unmanaged. Arrival, not age, is the signal.
func TestResolvesWhenTheCarPluggedInAfterItsLastPositionFix(t *testing.T) {
	v := posReady()
	fixed := posNow.Add(-6 * time.Minute) // in transit, judged away
	plugged := posNow.Add(-4 * time.Minute)
	v.AtHomeAt, v.PluggedInAt = &fixed, &plugged

	d := DecidePositionFix(v, nil, posOptions(), posNow)
	if !d.Resolve {
		t.Errorf("expected a resolve for a car that plugged in after its position was fixed: %s", d.Reason)
	}
}

// A car sitting plugged in since before its last position fix has nothing new to say.
func TestDoesNotResolveWhenThePositionPostdatesThePlugIn(t *testing.T) {
	v := posReady()
	plugged := posNow.Add(-2 * time.Hour)
	fixed := posNow.Add(-30 * time.Minute) // reported after it was already parked
	v.AtHomeAt, v.PluggedInAt = &fixed, &plugged

	if d := DecidePositionFix(v, nil, posOptions(), posNow); d.Resolve {
		t.Errorf("expected no resolve; the position already describes where it is parked: %s", d.Reason)
	}
}

// Arrival still does not override the gates that protect the car itself.
func TestArrivalDoesNotOverrideTheOnlineGate(t *testing.T) {
	offline := false
	v := posReady()
	fixed := posNow.Add(-6 * time.Minute)
	plugged := posNow.Add(-4 * time.Minute)
	v.AtHomeAt, v.PluggedInAt, v.Online = &fixed, &plugged, &offline

	if d := DecidePositionFix(v, nil, posOptions(), posNow); d.Resolve {
		t.Errorf("expected no resolve for a sleeping car: %s", d.Reason)
	}
}
