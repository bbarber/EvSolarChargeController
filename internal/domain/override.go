package domain

import "time"

// Observation is a telemetry frame reduced to the fields the override rule needs.
//
// Every field is optional because Tesla streams each signal on change rather than sending a
// complete snapshot, so a frame carrying one field must not wipe the others.
type Observation struct {
	VIN        string
	ObservedAt time.Time

	// ReportedAmps is the vehicle's configured charge current (Tesla field ChargeAmps, 49).
	ReportedAmps *int

	// ReportedMaxAmps is the vehicle-reported ceiling (ChargeCurrentRequestMax, 54).
	ReportedMaxAmps *int

	BatteryLevelPercent *int

	ChargingState *ChargingState

	// LatchDisengaged is a second unplug signal, from ChargePortLatch (118).
	LatchDisengaged *bool
}

// ApplyObservation folds a telemetry frame into the stored state and applies the manual-override
// rule: if the car reports a charge current this controller did not set, a person moved the slider
// in the app, so automatic adjustment stops until the car unplugs.
//
// Mutates and returns state.
func ApplyObservation(state *VehicleState, obs Observation, opts ChargingOptions) *VehicleState {
	if obs.ChargingState != nil {
		state.ChargingState = *obs.ChargingState
	}
	if obs.ReportedMaxAmps != nil && *obs.ReportedMaxAmps > 0 {
		state.ReportedMaxAmps = intPtr(*obs.ReportedMaxAmps)
	}
	if obs.ReportedAmps != nil {
		state.ChargeAmps = intPtr(*obs.ReportedAmps)
	}
	if soc := obs.BatteryLevelPercent; soc != nil && *soc >= 0 && *soc <= 100 {
		state.BatteryLevelPercent = intPtr(*soc)
	}

	effective := state.ChargingState
	unplugged := effective.IsUnplugged() || (obs.LatchDisengaged != nil && *obs.LatchDisengaged)

	switch {
	case unplugged:
		// Unplugging is the reset point for the whole state machine. What we last set is
		// forgotten too, since the next session starts from whatever the car defaults to.
		state.OverrideActive = false
		state.OverrideDetectedAt = nil
		state.LastSetAmps = nil
		state.LastSetAt = nil
		state.SocStopIssuedAt = nil
		state.LowSolarStopIssuedAt = nil

	case shouldFlagOverride(state, obs, opts, effective):
		state.OverrideActive = true
		state.OverrideDetectedAt = timePtr(obs.ObservedAt)
	}

	state.LastUpdated = obs.ObservedAt
	return state
}

func shouldFlagOverride(state *VehicleState, obs Observation, opts ChargingOptions, effective ChargingState) bool {
	if state.OverrideActive {
		return false // Already flagged; nothing to re-evaluate until unplug.
	}

	// We stopped the session and the car is charging again while our marker is still set. A resume
	// by this controller clears the marker first, so reaching here means a person restarted it —
	// hand back control rather than stopping them again every cycle.
	stopMarker := state.SocStopIssuedAt
	if stopMarker == nil {
		stopMarker = state.LowSolarStopIssuedAt
	}
	if stopMarker != nil && effective.IsActivelyCharging() &&
		obs.ObservedAt.Sub(*stopMarker) >= opts.OverrideSettleWindow {
		return true
	}

	if obs.ReportedAmps == nil {
		return false // This frame carried no amp value.
	}
	if state.LastSetAmps == nil {
		return false // We have never set a value, so nothing to contradict.
	}
	if *obs.ReportedAmps == *state.LastSetAmps {
		return false
	}

	// Only a charging vehicle can meaningfully contradict us. A stopped or complete car reports
	// transitional values that should not be read as user intent.
	if !effective.IsActivelyCharging() {
		return false
	}

	// Telemetry emitted before our command landed still carries the old value.
	if state.LastSetAt != nil && obs.ObservedAt.Sub(*state.LastSetAt) < opts.OverrideSettleWindow {
		return false
	}

	return true
}
