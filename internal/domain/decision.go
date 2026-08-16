package domain

import (
	"fmt"
	"math"
	"time"
)

type ChargeAction int

const (
	// ActionSetAmps issues a set_charging_amps command.
	ActionSetAmps ChargeAction = iota

	// ActionStopCharging stops the session because the state-of-charge cap was reached. This
	// controller does not charge past the cap even when the vehicle's own limit is higher.
	ActionStopCharging

	// ActionSkipAtSocCap means at or above the cap and already stopped; nothing to do.
	ActionSkipAtSocCap

	// ActionStopChargingLowSolar stops because solar cannot sustain even the minimum current.
	// Stopping beats clamping up to the minimum, which would draw the shortfall from the grid.
	ActionStopChargingLowSolar

	// ActionResumeCharging resumes a session this controller stopped for low solar.
	ActionResumeCharging

	// ActionStartCharging begins a session on a car that is plugged in and idle but that this
	// controller did not stop. Only reachable when StartWhenPluggedIn is set.
	ActionStartCharging

	// ActionSkipInsufficientSolar means not enough solar, and not currently charging.
	ActionSkipInsufficientSolar

	// ActionSkipNoVehicleState means no telemetry has arrived, or it is too old to trust.
	ActionSkipNoVehicleState

	// ActionSkipNotCharging means the vehicle is not actively charging. Never send a command
	// here — it could wake the car.
	ActionSkipNotCharging

	// ActionSkipOverrideActive means a person changed amps in the app; hands off until unplug.
	ActionSkipOverrideActive

	// ActionSkipNoSolarData means no usable readings in the trailing window.
	ActionSkipNoSolarData

	// ActionSkipAlreadyAtTarget means the target equals what we already set.
	ActionSkipAlreadyAtTarget
)

var chargeActionNames = map[ChargeAction]string{
	ActionSetAmps:               "SetAmps",
	ActionStopCharging:          "StopCharging",
	ActionSkipAtSocCap:          "SkipAtSocCap",
	ActionStopChargingLowSolar:  "StopChargingLowSolar",
	ActionResumeCharging:        "ResumeCharging",
	ActionStartCharging:         "StartCharging",
	ActionSkipInsufficientSolar: "SkipInsufficientSolar",
	ActionSkipNoVehicleState:    "SkipNoVehicleState",
	ActionSkipNotCharging:       "SkipNotCharging",
	ActionSkipOverrideActive:    "SkipOverrideActive",
	ActionSkipNoSolarData:       "SkipNoSolarData",
	ActionSkipAlreadyAtTarget:   "SkipAlreadyAtTarget",
}

func (a ChargeAction) String() string {
	if name, ok := chargeActionNames[a]; ok {
		return name
	}
	return "Unknown"
}

// Decision is the outcome of one sync evaluation, carrying a human-readable reason for the log.
type Decision struct {
	Action     ChargeAction
	TargetAmps *int
	Reason     string
}

func (d Decision) ShouldSend() bool { return d.Action == ActionSetAmps }

func (d Decision) ShouldStop() bool {
	return d.Action == ActionStopCharging || d.Action == ActionStopChargingLowSolar
}

func (d Decision) ShouldResume() bool { return d.Action == ActionResumeCharging }

// ShouldStart is the opt-in case: a car plugged in and idle that this controller did not stop.
// Handled the same way as a resume — start the session, then set the current.
func (d Decision) ShouldStart() bool { return d.Action == ActionStartCharging }

func skip(action ChargeAction, reason string) Decision {
	return Decision{Action: action, Reason: reason}
}

// Decide is the pure decision logic for the solar -> amps sync. It is free of I/O so the gating
// rules — never wake a sleeping car, respect manual overrides — are directly testable.
//
// Two different situations both produce no solar figure, and they demand opposite responses:
//
//   - solarWindowOpen and maxAmpsLastHour nil — the poll failed. Missing data is not evidence of
//     missing production, so a running session is left alone.
//   - !solarWindowOpen — the sun is down. This car charges on solar or not at all, so a running
//     session is stopped. It is recorded as a low-solar stop, which means it resumes by itself
//     when production returns the next morning.
func Decide(vehicle *VehicleState, maxAmpsLastHour *float64, solarWindowOpen bool, opts ChargingOptions, now time.Time) Decision {
	if vehicle == nil {
		return skip(ActionSkipNoVehicleState, "No telemetry received yet for any managed vehicle.")
	}

	age := now.Sub(vehicle.LastUpdated)
	if age > opts.VehicleStateStaleAfter {
		return skip(ActionSkipNoVehicleState, fmt.Sprintf(
			"Telemetry for %s is %.1fh old (stale after %.0fh); assuming asleep.",
			vehicle.VIN, age.Hours(), opts.VehicleStateStaleAfter.Hours()))
	}

	if vehicle.OverrideActive {
		detected := "unknown"
		if vehicle.OverrideDetectedAt != nil {
			detected = vehicle.OverrideDetectedAt.Format(time.RFC3339Nano)
		}
		return skip(ActionSkipOverrideActive, fmt.Sprintf(
			"Manual override active for %s since %s; waiting for unplug.", vehicle.VIN, detected))
	}

	state := vehicle.ChargingState

	// The cap is checked before the charging test so an already-stopped car at the cap reports the
	// real reason rather than a generic "not charging".
	if soc := vehicle.BatteryLevelPercent; soc != nil && *soc >= opts.MaxSocPercent {
		if !state.IsActivelyCharging() {
			return skip(ActionSkipAtSocCap, fmt.Sprintf(
				"%s is at %d%% (cap %d%%) and not charging.", vehicle.VIN, *soc, opts.MaxSocPercent))
		}
		return Decision{Action: ActionStopCharging, Reason: fmt.Sprintf(
			"%s reached %d%%, at or above the %d%% cap; stopping the charge session.",
			vehicle.VIN, *soc, opts.MaxSocPercent)}
	}

	// The sun is down. Nothing this controller can match, so nothing should be drawing.
	if !solarWindowOpen {
		if state.IsActivelyCharging() {
			return Decision{Action: ActionStopChargingLowSolar, Reason: fmt.Sprintf(
				"Outside the solar window; stopping %s rather than charging %s from the grid.",
				vehicle.VIN, "overnight")}
		}
		return skip(ActionSkipInsufficientSolar,
			fmt.Sprintf("Outside the solar window; leaving %s stopped.", vehicle.VIN))
	}

	if maxAmpsLastHour == nil {
		return skip(ActionSkipNoSolarData,
			"No solar readings inside the trailing window; leaving amps unchanged.")
	}
	amps := *maxAmpsLastHour

	// Whether solar alone can sustain the minimum. Rounded before the clamp, because clamping *up*
	// to the minimum is exactly the grid draw we are trying to avoid.
	if int(math.Round(amps)) < opts.MinChargeAmps {
		// Taking the maximum over the trailing window already damps this heavily: a passing cloud
		// cannot trip it, only a sustained loss of production.
		if state.IsActivelyCharging() {
			return Decision{Action: ActionStopChargingLowSolar, Reason: fmt.Sprintf(
				"Solar peaked at %.2fA over the trailing window, below the %dA minimum; "+
					"stopping %s rather than drawing the shortfall from the grid.",
				amps, opts.MinChargeAmps, vehicle.VIN)}
		}
		return skip(ActionSkipInsufficientSolar, fmt.Sprintf(
			"Solar peaked at %.2fA, below the %dA minimum; leaving %s stopped.",
			amps, opts.MinChargeAmps, vehicle.VIN))
	}

	target, err := ToRequestableAmps(amps, opts.MinChargeAmps, opts.MaxChargeAmps)
	if err != nil {
		return skip(ActionSkipNoSolarData, err.Error())
	}

	if !state.IsActivelyCharging() {
		// Resuming a session this controller stopped is always allowed: we know why it stopped.
		if vehicle.LowSolarStopIssuedAt != nil && state.IsPluggedIn() {
			return Decision{Action: ActionResumeCharging, TargetAmps: intPtr(target), Reason: fmt.Sprintf(
				"Solar recovered to %.2fA; resuming %s at %dA.", amps, vehicle.VIN, target)}
		}

		// Starting a session nobody asked us to start is opt-in. It is safe here and nowhere else:
		// the car is reporting telemetry and reports itself plugged in, so the usual worry — that a
		// command wakes a sleeping vehicle — does not apply. Every other gate has already passed.
		if opts.StartWhenPluggedIn && state.IsPluggedIn() {
			return Decision{Action: ActionStartCharging, TargetAmps: intPtr(target), Reason: fmt.Sprintf(
				"%s is plugged in and idle with %.2fA available; starting at %dA.",
				vehicle.VIN, amps, target)}
		}

		return skip(ActionSkipNotCharging, fmt.Sprintf(
			"%s is %s; not sending any command (avoids waking the vehicle).", vehicle.VIN, state))
	}

	// The car may cap below our configured ceiling for breaker or on-board-charger reasons. Respect
	// whatever it last reported, so we stop asking for current it will never accept — otherwise
	// every cycle would look like a mismatch and trip false override detection.
	if vm := vehicle.ReportedMaxAmps; vm != nil && *vm >= opts.MinChargeAmps && target > *vm {
		target = *vm
	}

	if vehicle.LastSetAmps != nil && *vehicle.LastSetAmps == target {
		return skip(ActionSkipAlreadyAtTarget, fmt.Sprintf(
			"Target %dA already set for %s.", target, vehicle.VIN))
	}

	previous := "unset"
	if vehicle.LastSetAmps != nil {
		previous = fmt.Sprintf("%d", *vehicle.LastSetAmps)
	}
	return Decision{Action: ActionSetAmps, TargetAmps: intPtr(target), Reason: fmt.Sprintf(
		"Solar max over trailing window %.2fA -> requesting %dA for %s (was %s).",
		amps, target, vehicle.VIN, previous)}
}
