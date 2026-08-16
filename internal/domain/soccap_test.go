package domain

import (
	"testing"
	"time"
)

// This controller must never drive charging past the configured state of charge, even when the
// vehicle's own charge limit is set higher. A person can still charge past it by hand, which is
// treated as a manual override.

func socOptions() ChargingOptions {
	return ChargingOptions{
		MinChargeAmps:          5,
		MaxChargeAmps:          16,
		MaxSocPercent:          80,
		OverrideSettleWindow:   3 * time.Minute,
		VehicleStateStaleAfter: 6 * time.Hour,
	}
}

func socVehicle(soc *int, opts ...vehOpt) *VehicleState {
	v := &VehicleState{
		VIN:                 testVIN,
		ChargingState:       StateCharging,
		BatteryLevelPercent: soc,
		LastUpdated:         testNow.Add(-2 * time.Minute),
	}
	for _, o := range opts {
		o(v)
	}
	return v
}

func vehSocStop(at time.Time) vehOpt {
	return func(v *VehicleState) { v.SocStopIssuedAt = &at }
}

func TestStopsChargingAtOrAboveTheCap(t *testing.T) {
	for _, soc := range []int{80, 81, 100} {
		d := Decide(socVehicle(intPtr(soc)), amps(16), true, socOptions(), testNow)

		if d.Action != ActionStopCharging {
			t.Errorf("soc %d: Action = %v, want StopCharging", soc, d.Action)
		}
		if !d.ShouldStop() || d.ShouldSend() {
			t.Errorf("soc %d: expected a stop and no set", soc)
		}
	}
}

func TestChargesNormallyBelowTheCap(t *testing.T) {
	for _, soc := range []int{0, 50, 79} {
		d := Decide(socVehicle(intPtr(soc)), amps(12), true, socOptions(), testNow)

		if d.Action != ActionSetAmps {
			t.Errorf("soc %d: Action = %v, want SetAmps", soc, d.Action)
		}
		if d.TargetAmps == nil || *d.TargetAmps != 12 {
			t.Errorf("soc %d: TargetAmps = %v, want 12", soc, d.TargetAmps)
		}
	}
}

func TestDoesNotSendAStopWhenAlreadyNotChargingAtTheCap(t *testing.T) {
	d := Decide(socVehicle(intPtr(85), vehState(StateComplete)), amps(16), true, socOptions(), testNow)

	if d.Action != ActionSkipAtSocCap {
		t.Errorf("Action = %v, want SkipAtSocCap", d.Action)
	}
	if d.ShouldStop() {
		t.Error("expected no stop command")
	}
}

func TestIgnoresTheCapWhenStateOfChargeIsUnknown(t *testing.T) {
	// Telemetry streams on change, so SoC may be absent early in a session. Refusing to charge on
	// missing data would strand the car; the vehicle's own limit still applies.
	if d := Decide(socVehicle(nil), amps(12), true, socOptions(), testNow); d.Action != ActionSetAmps {
		t.Errorf("Action = %v, want SetAmps", d.Action)
	}
}

func TestAManualOverrideOutranksTheCap(t *testing.T) {
	d := Decide(socVehicle(intPtr(95), vehOverride()), amps(16), true, socOptions(), testNow)

	if d.Action != ActionSkipOverrideActive {
		t.Errorf("Action = %v, want SkipOverrideActive", d.Action)
	}
	if d.ShouldStop() {
		t.Error("expected no stop command")
	}
}

func TestACustomCapIsHonoured(t *testing.T) {
	opts := socOptions()
	opts.MaxSocPercent = 60

	if d := Decide(socVehicle(intPtr(65)), amps(16), true, opts, testNow); d.Action != ActionStopCharging {
		t.Errorf("soc 65 against a 60%% cap: Action = %v, want StopCharging", d.Action)
	}
	if d := Decide(socVehicle(intPtr(55)), amps(16), true, opts, testNow); d.Action != ActionSetAmps {
		t.Errorf("soc 55 against a 60%% cap: Action = %v, want SetAmps", d.Action)
	}
}

func TestRestartingAfterOurStopIsTreatedAsAManualOverride(t *testing.T) {
	// Nothing in this controller restarts a charge, so if the car is charging again after we
	// stopped it, a person did it deliberately.
	s := socVehicle(intPtr(85), vehState(StateStopped), vehSocStop(testNow.Add(-30*time.Minute)))

	got := ApplyObservation(s, Observation{VIN: testVIN, ObservedAt: testNow,
		ChargingState: func() *ChargingState { c := StateCharging; return &c }()}, socOptions())

	if !got.OverrideActive {
		t.Error("expected a manual restart to be flagged as an override")
	}
}

func TestTelemetryArrivingJustAfterOurStopIsNotAnOverride(t *testing.T) {
	// The stop takes a moment to land; frames still in flight say Charging.
	s := socVehicle(intPtr(85), vehState(StateCharging), vehSocStop(testNow.Add(-20*time.Second)))

	got := ApplyObservation(s, Observation{VIN: testVIN, ObservedAt: testNow,
		ChargingState: func() *ChargingState { c := StateCharging; return &c }()}, socOptions())

	if got.OverrideActive {
		t.Error("expected in-flight telemetry not to trip the override")
	}
}

func TestUnpluggingClearsTheStopMarker(t *testing.T) {
	s := socVehicle(intPtr(85), vehState(StateComplete), vehSocStop(testNow.Add(-30*time.Minute)))

	got := ApplyObservation(s, Observation{VIN: testVIN, ObservedAt: testNow,
		ChargingState: func() *ChargingState { c := StateDisconnected; return &c }()}, socOptions())

	if got.SocStopIssuedAt != nil {
		t.Error("expected the stop marker to be cleared on unplug")
	}
	if got.OverrideActive {
		t.Error("expected no override after unplug")
	}
}

func TestStateOfChargeIsRecordedFromTelemetry(t *testing.T) {
	s := socVehicle(nil, vehState(StateCharging))

	got := ApplyObservation(s, Observation{
		VIN: testVIN, ObservedAt: testNow, BatteryLevelPercent: intPtr(72),
	}, socOptions())

	if got.BatteryLevelPercent == nil || *got.BatteryLevelPercent != 72 {
		t.Errorf("BatteryLevelPercent = %v, want 72", got.BatteryLevelPercent)
	}
}

func TestImplausibleStateOfChargeReadingsAreIgnored(t *testing.T) {
	for _, soc := range []int{-5, 101} {
		s := socVehicle(intPtr(70), vehState(StateCharging))

		got := ApplyObservation(s, Observation{
			VIN: testVIN, ObservedAt: testNow, BatteryLevelPercent: intPtr(soc),
		}, socOptions())

		if got.BatteryLevelPercent == nil || *got.BatteryLevelPercent != 70 {
			t.Errorf("soc %d: BatteryLevelPercent = %v, want the previous 70", soc, got.BatteryLevelPercent)
		}
	}
}
