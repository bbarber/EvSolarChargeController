package domain

import (
	"testing"
	"time"
)

// When solar cannot cover the connector's minimum current, clamping up to that minimum would draw
// the shortfall from the grid — the opposite of the point. The session is stopped instead, and
// resumed once production recovers.

func lowSolarVehicle(opts ...vehOpt) *VehicleState {
	v := &VehicleState{
		VIN:                 testVIN,
		ChargingState:       StateCharging,
		BatteryLevelPercent: intPtr(50),
		LastUpdated:         testNow.Add(-2 * time.Minute),
	}
	for _, o := range opts {
		o(v)
	}
	return v
}

func vehLowSolarStop(at time.Time) vehOpt {
	return func(v *VehicleState) { v.Session = SessionStoppedForSun; v.SessionSince = &at }
}

func vehSoc(p int) vehOpt { return func(v *VehicleState) { v.BatteryLevelPercent = intPtr(p) } }

func TestStopsRatherThanChargingFromTheGrid(t *testing.T) {
	for _, a := range []float64{0, 1.2, 4.4} {
		d := Decide(lowSolarVehicle(), amps(a), true, socOptions(), testNow)

		if d.Action != ActionStopChargingLowSolar {
			t.Errorf("%.1fA: Action = %v, want StopChargingLowSolar", a, d.Action)
		}
		if !d.ShouldStop() {
			t.Errorf("%.1fA: expected ShouldStop", a)
		}
	}
}

func TestChargesWhenSolarRoundsUpToTheMinimum(t *testing.T) {
	// 4.6A of production against a 5A request is ~96W from the grid — not worth stopping over.
	d := Decide(lowSolarVehicle(), amps(4.6), true, socOptions(), testNow)

	if d.Action != ActionSetAmps {
		t.Fatalf("Action = %v, want SetAmps", d.Action)
	}
	if d.TargetAmps == nil || *d.TargetAmps != 5 {
		t.Errorf("TargetAmps = %v, want 5", d.TargetAmps)
	}
}

func TestStaysStoppedWhileSolarIsLow(t *testing.T) {
	v := lowSolarVehicle(vehState(StateStopped), vehLowSolarStop(testNow.Add(-time.Hour)))
	d := Decide(v, amps(2), true, socOptions(), testNow)

	if d.Action != ActionSkipInsufficientSolar {
		t.Errorf("Action = %v, want SkipInsufficientSolar", d.Action)
	}
	if d.ShouldStop() || d.ShouldResume() {
		t.Error("expected neither a stop nor a resume")
	}
}

func TestResumesOnceSolarRecovers(t *testing.T) {
	v := lowSolarVehicle(vehState(StateStopped), vehLowSolarStop(testNow.Add(-time.Hour)))
	d := Decide(v, amps(11), true, socOptions(), testNow)

	if d.Action != ActionResumeCharging || !d.ShouldResume() {
		t.Fatalf("Action = %v, want ResumeCharging", d.Action)
	}
	if d.TargetAmps == nil || *d.TargetAmps != 11 {
		t.Errorf("TargetAmps = %v, want 11", d.TargetAmps)
	}
}

func TestDoesNotResumeASessionThisControllerDidNotStop(t *testing.T) {
	// The user stopped it, or the car simply is not charging. A command here could wake it.
	v := lowSolarVehicle(vehState(StateStopped))
	if d := Decide(v, amps(12), true, socOptions(), testNow); d.Action != ActionSkipNotCharging {
		t.Errorf("Action = %v, want SkipNotCharging", d.Action)
	}
}

func TestDoesNotResumeAnUnpluggedVehicle(t *testing.T) {
	v := lowSolarVehicle(vehState(StateDisconnected), vehLowSolarStop(testNow.Add(-time.Hour)))
	if d := Decide(v, amps(12), true, socOptions(), testNow); d.Action != ActionSkipNotCharging {
		t.Errorf("Action = %v, want SkipNotCharging", d.Action)
	}
}

func TestDoesNotResumeWhileAnOverrideIsActive(t *testing.T) {
	v := lowSolarVehicle(vehState(StateStopped), vehLowSolarStop(testNow.Add(-time.Hour)), vehOverride())
	if d := Decide(v, amps(12), true, socOptions(), testNow); d.Action != ActionSkipOverrideActive {
		t.Errorf("Action = %v, want SkipOverrideActive", d.Action)
	}
}

func TestTheSocCapOutranksALowSolarResume(t *testing.T) {
	v := lowSolarVehicle(vehState(StateStopped), vehLowSolarStop(testNow.Add(-time.Hour)), vehSoc(90))
	if d := Decide(v, amps(12), true, socOptions(), testNow); d.Action != ActionSkipAtSocCap {
		t.Errorf("Action = %v, want SkipAtSocCap", d.Action)
	}
}

func TestRestartingByHandAfterALowSolarStopIsAnOverride(t *testing.T) {
	// A resume by this controller clears the marker first, so a still-set marker plus a charging
	// car means a person restarted it.
	s := lowSolarVehicle(vehState(StateStopped), vehLowSolarStop(testNow.Add(-30*time.Minute)))

	got := ApplyObservation(s, Observation{VIN: testVIN, ObservedAt: testNow,
		ChargingState: func() *ChargingState { c := StateCharging; return &c }()}, socOptions())

	if got.Session != SessionOverridden {
		t.Error("expected a manual restart to be flagged as an override")
	}
}

func TestUnpluggingClearsTheLowSolarMarker(t *testing.T) {
	s := lowSolarVehicle(vehState(StateStopped), vehLowSolarStop(testNow.Add(-30*time.Minute)))

	got := ApplyObservation(s, Observation{VIN: testVIN, ObservedAt: testNow,
		ChargingState: func() *ChargingState { c := StateDisconnected; return &c }()}, socOptions())

	if got.Session != SessionAuto {
		t.Errorf("Session = %v, want Auto after unplug", got.Session)
	}
}

func TestMissingSolarDataDoesNotStopARunningSession(t *testing.T) {
	// A failed Enphase poll is not evidence of low production.
	d := Decide(lowSolarVehicle(), nil, true, socOptions(), testNow)

	if d.Action != ActionSkipNoSolarData {
		t.Errorf("Action = %v, want SkipNoSolarData", d.Action)
	}
	if d.ShouldStop() {
		t.Error("expected no stop on missing data")
	}
}
