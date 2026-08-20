package domain

import (
	"math"
	"time"
)

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

	// Latitude/Longitude are the car's position (Location, 21). Consumed in the fold to compute
	// AtHome, then discarded — the coordinate itself is never stored.
	Latitude  *float64
	Longitude *float64

	// FastCharger is FastChargerPresent (39).
	FastCharger *bool
}

// ApplyObservation folds a telemetry frame into the stored state and applies the manual-override
// rule: if the car reports a charge current this controller did not set, a person moved the slider
// in the app, so automatic adjustment stops until the car unplugs.
//
// Mutates and returns state.
func ApplyObservation(state *VehicleState, obs Observation, opts ChargingOptions) *VehicleState {
	wasPluggedIn := state.ChargingState.IsPluggedIn()
	if obs.ChargingState != nil {
		state.ChargingState = *obs.ChargingState
	}
	// The moment the cable goes in. Compared against AtHomeAt, this is what distinguishes "the
	// car is away" from "the car was away when it last reported, and has since arrived".
	if !wasPluggedIn && state.ChargingState.IsPluggedIn() {
		at := obs.ObservedAt
		state.PluggedInAt = &at
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
	if obs.FastCharger != nil {
		v := *obs.FastCharger
		state.FastCharger = &v
	}
	if opts.HomeConfigured() && obs.Latitude != nil && obs.Longitude != nil {
		ApplyPositionFix(state, *obs.Latitude, *obs.Longitude, obs.ObservedAt, opts)
	}

	effective := state.ChargingState
	unplugged := effective.IsUnplugged() || (obs.LatchDisengaged != nil && *obs.LatchDisengaged)

	switch {
	case unplugged:
		// Unplugging is the reset point for the whole state machine. What we last set is
		// forgotten too, since the next session starts from whatever the car defaults to.
		state.Session = SessionAuto
		state.SessionSince = nil
		state.LastSetAmps = nil
		state.LastSetAt = nil

	case shouldFlagOverride(state, obs, opts, effective):
		state.Session = SessionOverridden
		state.SessionSince = timePtr(obs.ObservedAt)
	}

	state.LastUpdated = obs.ObservedAt
	return state
}

// ApplyPositionFix folds a position into the home boolean. The coordinate is consumed here and
// never stored: callers hand it in, the boolean and its timestamp are what survive.
func ApplyPositionFix(state *VehicleState, lat, lon float64, at time.Time, opts ChargingOptions) {
	if state == nil || !opts.HomeConfigured() {
		return
	}
	home := withinMeters(lat, lon, opts.HomeLatitude, opts.HomeLongitude, opts.HomeRadiusM)
	state.AtHome = &home
	state.AtHomeAt = &at
}

// MetersFromHome is the distance used by the home gate, for logging. It exists so an operator can
// see how a decision was reached without the position ever being recorded.
func MetersFromHome(lat, lon float64, opts ChargingOptions) float64 {
	const mPerDegLat = 111320.0
	dLat := (lat - opts.HomeLatitude) * mPerDegLat
	dLon := (lon - opts.HomeLongitude) * mPerDegLat * math.Cos(opts.HomeLatitude*math.Pi/180)
	return math.Sqrt(dLat*dLat + dLon*dLon)
}

// withinMeters is an equirectangular distance test — ample at a 150m radius, where the error
// against a true great-circle distance is far below GPS noise.
func withinMeters(lat, lon, homeLat, homeLon, radiusM float64) bool {
	const mPerDegLat = 111320.0
	dLat := (lat - homeLat) * mPerDegLat
	dLon := (lon - homeLon) * mPerDegLat * math.Cos(homeLat*math.Pi/180)
	return dLat*dLat+dLon*dLon <= radiusM*radiusM
}

func shouldFlagOverride(state *VehicleState, obs Observation, opts ChargingOptions, effective ChargingState) bool {
	if state.Session == SessionOverridden {
		return false // Already flagged; nothing to re-evaluate until unplug.
	}

	// We stopped the session and the car is charging again. A resume by this controller moves the
	// session to Auto first, so a StoppedByUs state with a charging car means a person restarted
	// it — hand back control rather than stopping them again every cycle. The settle window
	// tolerates frames that were in flight when our stop landed.
	if state.Session.StoppedByUs() && effective.IsActivelyCharging() &&
		state.SessionSince != nil &&
		obs.ObservedAt.Sub(*state.SessionSince) >= opts.OverrideSettleWindow {
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
