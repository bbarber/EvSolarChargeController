package domain

import (
	"testing"
	"time"
)

func amps(v float64) *float64 { return &v }

func decisionOptions() ChargingOptions {
	return ChargingOptions{
		SystemVoltage:          240,
		MinChargeAmps:          5,
		MaxChargeAmps:          16,
		VehicleStateStaleAfter: 6 * time.Hour,
	}
}

type vehOpt func(*VehicleState)

func vehState(c ChargingState) vehOpt { return func(v *VehicleState) { v.ChargingState = c } }
func vehOverride() vehOpt             { return func(v *VehicleState) { v.OverrideActive = true } }
func vehLastSet(a int) vehOpt         { return func(v *VehicleState) { v.LastSetAmps = intPtr(a) } }
func vehReportedMax(a int) vehOpt     { return func(v *VehicleState) { v.ReportedMaxAmps = intPtr(a) } }
func vehUpdated(t time.Time) vehOpt   { return func(v *VehicleState) { v.LastUpdated = t } }

func decisionVehicle(opts ...vehOpt) *VehicleState {
	v := &VehicleState{
		VIN:           testVIN,
		ChargingState: StateCharging,
		LastUpdated:   testNow.Add(-2 * time.Minute),
	}
	for _, o := range opts {
		o(v)
	}
	return v
}

func TestSetsAmpsFromTheTrailingWindowMaximum(t *testing.T) {
	d := Decide(decisionVehicle(), amps(12.3), true, decisionOptions(), testNow)

	if d.Action != ActionSetAmps {
		t.Fatalf("Action = %v, want SetAmps", d.Action)
	}
	if d.TargetAmps == nil || *d.TargetAmps != 12 {
		t.Errorf("TargetAmps = %v, want 12", d.TargetAmps)
	}
	if !d.ShouldSend() {
		t.Error("expected ShouldSend")
	}
}

func TestSkipsWhenNoTelemetryHasArrived(t *testing.T) {
	d := Decide(nil, amps(12), true, decisionOptions(), testNow)

	if d.Action != ActionSkipNoVehicleState {
		t.Errorf("Action = %v, want SkipNoVehicleState", d.Action)
	}
	if d.ShouldSend() {
		t.Error("expected no command")
	}
}

func TestSkipsWhenTelemetryIsStaleBecauseTheCarIsProbablyAsleep(t *testing.T) {
	stale := decisionVehicle(vehUpdated(testNow.Add(-9 * time.Hour)))

	if d := Decide(stale, amps(12), true, decisionOptions(), testNow); d.Action != ActionSkipNoVehicleState {
		t.Errorf("Action = %v, want SkipNoVehicleState", d.Action)
	}
}

func TestNeverCommandsAVehicleThatIsNotActivelyCharging(t *testing.T) {
	for _, state := range []ChargingState{
		StateDisconnected, StateComplete, StateStopped, StateNoPower, StateUnknown,
	} {
		t.Run(state.String(), func(t *testing.T) {
			d := Decide(decisionVehicle(vehState(state)), amps(12), true, decisionOptions(), testNow)

			if d.Action != ActionSkipNotCharging {
				t.Errorf("Action = %v, want SkipNotCharging", d.Action)
			}
			if d.ShouldSend() {
				t.Error("expected no command — sending one could wake the vehicle")
			}
		})
	}
}

func TestRespectsAnActiveManualOverride(t *testing.T) {
	if d := Decide(decisionVehicle(vehOverride()), amps(12), true, decisionOptions(), testNow); d.Action != ActionSkipOverrideActive {
		t.Errorf("Action = %v, want SkipOverrideActive", d.Action)
	}
}

func TestOverrideTakesPrecedenceOverEverythingExceptStaleness(t *testing.T) {
	v := decisionVehicle(vehOverride(), vehLastSet(5))

	if d := Decide(v, amps(16), true, decisionOptions(), testNow); d.Action != ActionSkipOverrideActive {
		t.Errorf("Action = %v, want SkipOverrideActive", d.Action)
	}
}

func TestSkipsWhenTheWindowHoldsNoReadings(t *testing.T) {
	if d := Decide(decisionVehicle(), nil, true, decisionOptions(), testNow); d.Action != ActionSkipNoSolarData {
		t.Errorf("Action = %v, want SkipNoSolarData", d.Action)
	}
}

func TestSkipsARedundantCommandWhenAlreadyAtTarget(t *testing.T) {
	d := Decide(decisionVehicle(vehLastSet(12)), amps(12.1), true, decisionOptions(), testNow)

	if d.Action != ActionSkipAlreadyAtTarget {
		t.Errorf("Action = %v, want SkipAlreadyAtTarget", d.Action)
	}
}

func TestStopsInsteadOfClampingUpWhenProductionCannotCoverTheMinimum(t *testing.T) {
	// Clamping 0.7A up to the 5A minimum would pull the remaining ~1kW from the grid.
	d := Decide(decisionVehicle(), amps(0.7), true, decisionOptions(), testNow)

	if d.Action != ActionStopChargingLowSolar {
		t.Errorf("Action = %v, want StopChargingLowSolar", d.Action)
	}
	if d.TargetAmps != nil {
		t.Errorf("TargetAmps = %v, want nil", d.TargetAmps)
	}
}

func TestClampsDownToTheConfiguredCeiling(t *testing.T) {
	d := Decide(decisionVehicle(), amps(48), true, decisionOptions(), testNow)

	if d.TargetAmps == nil || *d.TargetAmps != 16 {
		t.Errorf("TargetAmps = %v, want 16", d.TargetAmps)
	}
}

func TestHonoursALowerVehicleReportedMaximum(t *testing.T) {
	// Asking for more than the car will accept would make every cycle look like a mismatch and
	// falsely trip override detection.
	d := Decide(decisionVehicle(vehReportedMax(12)), amps(16), true, decisionOptions(), testNow)

	if d.TargetAmps == nil || *d.TargetAmps != 12 {
		t.Errorf("TargetAmps = %v, want 12", d.TargetAmps)
	}
}

func TestIgnoresAnImplausibleVehicleReportedMaximum(t *testing.T) {
	d := Decide(decisionVehicle(vehReportedMax(0)), amps(16), true, decisionOptions(), testNow)

	if d.TargetAmps == nil || *d.TargetAmps != 16 {
		t.Errorf("TargetAmps = %v, want 16", d.TargetAmps)
	}
}
