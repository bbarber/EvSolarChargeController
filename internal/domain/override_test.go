package domain

import (
	"testing"
	"time"
)

const testVIN = "5YJ3E1EA7KF000001"

var testNow = time.Date(2026, 8, 11, 14, 0, 0, 0, time.UTC)

func overrideOptions() ChargingOptions {
	return ChargingOptions{OverrideSettleWindow: 3 * time.Minute}
}

type stateOpt func(*VehicleState)

func withLastSet(amps int, at *time.Time) stateOpt {
	return func(s *VehicleState) { s.LastSetAmps = intPtr(amps); s.LastSetAt = at }
}
func withOverrideActive() stateOpt       { return func(s *VehicleState) { s.Session = SessionOverridden } }
func withState(c ChargingState) stateOpt { return func(s *VehicleState) { s.ChargingState = c } }

func newState(opts ...stateOpt) *VehicleState {
	s := &VehicleState{
		VIN:           testVIN,
		ChargingState: StateCharging,
		LastUpdated:   testNow.Add(-10 * time.Minute),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

type obsOpt func(*Observation)

func obsAmps(a int) obsOpt            { return func(o *Observation) { o.ReportedAmps = intPtr(a) } }
func obsMaxAmps(a int) obsOpt         { return func(o *Observation) { o.ReportedMaxAmps = intPtr(a) } }
func obsState(c ChargingState) obsOpt { return func(o *Observation) { o.ChargingState = &c } }
func obsLatchDisengaged() obsOpt {
	return func(o *Observation) { v := true; o.LatchDisengaged = &v }
}
func obsAt(t time.Time) obsOpt { return func(o *Observation) { o.ObservedAt = t } }

func newObs(opts ...obsOpt) Observation {
	o := Observation{VIN: testVIN, ObservedAt: testNow}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

func ago(d time.Duration) *time.Time { t := testNow.Add(-d); return &t }

func TestFlagsAnOverrideWhenReportedAmpsContradictWhatWeSet(t *testing.T) {
	s := newState(withLastSet(12, ago(30*time.Minute)))

	got := ApplyObservation(s, newObs(obsAmps(32), obsState(StateCharging)), overrideOptions())

	if got.Session != SessionOverridden {
		t.Errorf("Session = %v, want Overridden", got.Session)
	}
	if got.SessionSince == nil || !got.SessionSince.Equal(testNow) {
		t.Errorf("SessionSince = %v, want %v", got.SessionSince, testNow)
	}
	if got.ChargeAmps == nil || *got.ChargeAmps != 32 {
		t.Errorf("ChargeAmps = %v, want 32", got.ChargeAmps)
	}
}

func TestDoesNotFlagWhenTheCarConfirmsOurValue(t *testing.T) {
	s := newState(withLastSet(12, ago(30*time.Minute)))
	if ApplyObservation(s, newObs(obsAmps(12), obsState(StateCharging)), overrideOptions()).Session == SessionOverridden {
		t.Error("expected no override when the car confirms our value")
	}
}

func TestDoesNotFlagInsideTheSettleWindow(t *testing.T) {
	// Telemetry emitted before our command landed still carries the previous value.
	s := newState(withLastSet(12, ago(30*time.Second)))
	if ApplyObservation(s, newObs(obsAmps(8), obsState(StateCharging)), overrideOptions()).Session == SessionOverridden {
		t.Error("expected no override inside the settle window")
	}
}

func TestDoesNotFlagBeforeWeHaveEverSetAValue(t *testing.T) {
	s := newState()
	if ApplyObservation(s, newObs(obsAmps(24), obsState(StateCharging)), overrideOptions()).Session == SessionOverridden {
		t.Error("expected no override when we have never set a value")
	}
}

func TestDoesNotFlagWhenTheVehicleIsNotCharging(t *testing.T) {
	s := newState(withLastSet(12, ago(30*time.Minute)))
	if ApplyObservation(s, newObs(obsAmps(8), obsState(StateStopped)), overrideOptions()).Session == SessionOverridden {
		t.Error("expected no override when the vehicle is not charging")
	}
}

func TestUnpluggingClearsTheOverrideAndForgetsTheLastSetValue(t *testing.T) {
	s := newState(withLastSet(12, ago(30*time.Minute)), withOverrideActive())

	got := ApplyObservation(s, newObs(obsState(StateDisconnected)), overrideOptions())

	if got.Session != SessionAuto || got.SessionSince != nil || got.LastSetAmps != nil || got.LastSetAt != nil {
		t.Errorf("expected a full reset on unplug, got %+v", got)
	}
}

func TestADisengagedLatchAlsoClearsTheOverride(t *testing.T) {
	s := newState(withLastSet(12, nil), withOverrideActive(), withState(StateStopped))
	if ApplyObservation(s, newObs(obsLatchDisengaged()), overrideOptions()).Session == SessionOverridden {
		t.Error("expected a disengaged latch to clear the override")
	}
}

func TestAnOverrideSurvivesAPauseInCharging(t *testing.T) {
	// Charging stopping is not unplugging — the user's setting should still be respected.
	s := newState(withLastSet(12, nil), withOverrideActive())
	if ApplyObservation(s, newObs(obsState(StateStopped)), overrideOptions()).Session != SessionOverridden {
		t.Error("expected the override to survive a pause")
	}
}

func TestAnOverrideIsNotReEvaluatedWhileAlreadyActive(t *testing.T) {
	s := newState(withLastSet(12, ago(time.Hour)), withOverrideActive())
	detected := testNow.Add(-20 * time.Minute)
	s.SessionSince = &detected

	got := ApplyObservation(s, newObs(obsAmps(20), obsState(StateCharging)), overrideOptions())

	if got.Session != SessionOverridden {
		t.Error("expected the override to remain active")
	}
	if !got.SessionSince.Equal(detected) {
		t.Errorf("SessionSince moved to %v, want %v", got.SessionSince, detected)
	}
}

func TestAbsentFieldsLeavePreviousValuesIntact(t *testing.T) {
	// Tesla streams signals on change, so a frame carrying one field must not wipe the others.
	s := newState(withLastSet(12, nil), withState(StateCharging))
	s.ChargeAmps = intPtr(12)
	s.ReportedMaxAmps = intPtr(16)

	got := ApplyObservation(s, newObs(), overrideOptions())

	if got.ChargeAmps == nil || *got.ChargeAmps != 12 {
		t.Errorf("ChargeAmps = %v, want 12", got.ChargeAmps)
	}
	if got.ReportedMaxAmps == nil || *got.ReportedMaxAmps != 16 {
		t.Errorf("ReportedMaxAmps = %v, want 16", got.ReportedMaxAmps)
	}
	if got.ChargingState != StateCharging {
		t.Errorf("ChargingState = %v, want Charging", got.ChargingState)
	}
}

func TestZeroReportedMaxIsIgnored(t *testing.T) {
	s := newState()
	s.ReportedMaxAmps = intPtr(16)

	got := ApplyObservation(s, newObs(obsMaxAmps(0)), overrideOptions())

	if got.ReportedMaxAmps == nil || *got.ReportedMaxAmps != 16 {
		t.Errorf("ReportedMaxAmps = %v, want the previous 16", got.ReportedMaxAmps)
	}
}

func TestRecordsTheObservationTime(t *testing.T) {
	observedAt := testNow.Add(-time.Minute)
	got := ApplyObservation(newState(), newObs(obsAt(observedAt), obsState(StateCharging)), overrideOptions())

	if !got.LastUpdated.Equal(observedAt) {
		t.Errorf("LastUpdated = %v, want %v", got.LastUpdated, observedAt)
	}
}

func TestAFullSessionRunsOverrideThenReset(t *testing.T) {
	opts := overrideOptions()
	s := newState(withLastSet(10, ago(30*time.Minute)))

	// A person bumps amps in the app.
	s = ApplyObservation(s, newObs(obsAmps(32), obsState(StateCharging)), opts)
	if s.Session != SessionOverridden {
		t.Fatal("expected the override to be flagged")
	}

	// Charging completes — the override must survive, since the cable is still in.
	s = ApplyObservation(s, newObs(obsState(StateComplete), obsAt(testNow.Add(time.Hour))), opts)
	if s.Session != SessionOverridden {
		t.Fatal("expected the override to survive completion")
	}

	// Cable comes out — back under automatic control.
	s = ApplyObservation(s, newObs(obsState(StateDisconnected), obsAt(testNow.Add(2*time.Hour))), opts)
	if s.Session != SessionAuto || s.LastSetAmps != nil {
		t.Errorf("expected a reset on unplug, got %+v", s)
	}
}

// Plugging in is recorded so the home gate can tell "away" from "was away, and has since arrived".
func TestPlugInIsTimestamped(t *testing.T) {
	opts := DefaultChargingOptions()
	at := time.Date(2026, 8, 20, 21, 56, 0, 0, time.UTC)

	v := NewVehicleState("VIN1", at)
	v.ChargingState = StateDisconnected

	charging := StateCharging
	ApplyObservation(v, Observation{VIN: "VIN1", ObservedAt: at, ChargingState: &charging}, opts)
	if v.PluggedInAt == nil || !v.PluggedInAt.Equal(at) {
		t.Fatalf("PluggedInAt = %v, want %v", v.PluggedInAt, at)
	}

	// Still plugged in a frame later: the original arrival time must survive.
	later := at.Add(5 * time.Minute)
	stopped := StateStopped
	ApplyObservation(v, Observation{VIN: "VIN1", ObservedAt: later, ChargingState: &stopped}, opts)
	if !v.PluggedInAt.Equal(at) {
		t.Errorf("PluggedInAt moved to %v; it should still be the moment of arrival %v", v.PluggedInAt, at)
	}
}
