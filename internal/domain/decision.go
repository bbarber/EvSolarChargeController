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

	// Freshness is measured across both channels. Signals transmit on change, so a car that just
	// answered a wake may send no telemetry at all — its connectivity event is the proof of life.
	lastHeard := vehicle.LastUpdated
	if vehicle.OnlineAt != nil && vehicle.OnlineAt.After(lastHeard) {
		lastHeard = *vehicle.OnlineAt
	}

	// A car connectivity says is offline is not "unknown" — it is asleep, and its silence is
	// explained. Charge state cannot change while asleep (unplugging wakes the car, which would
	// have produced an event), so the stored state remains trustworthy overnight. Without this
	// bypass, a car plugged in on Thursday night is unreachable all Friday morning: too stale to
	// consider, and the wake path sits behind a gate staleness never lets it reach.
	knownAsleep := vehicle.Online != nil && !*vehicle.Online

	age := now.Sub(lastHeard)
	if age > opts.VehicleStateStaleAfter && !knownAsleep {
		return skip(ActionSkipNoVehicleState, fmt.Sprintf(
			"Nothing heard from %s for %.1fh (stale after %.0fh) and connectivity does not say asleep; not trusting the stored state.",
			vehicle.VIN, age.Hours(), opts.VehicleStateStaleAfter.Hours()))
	}

	if vehicle.Session == SessionOverridden {
		since := "unknown"
		if vehicle.SessionSince != nil {
			since = vehicle.SessionSince.Format(time.RFC3339Nano)
		}
		return skip(ActionSkipOverrideActive, fmt.Sprintf(
			"Manual override active for %s since %s; waiting for unplug.", vehicle.VIN, since))
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
		// Resuming a session this controller stopped for sun is allowed without a connectivity
		// event — unless connectivity affirmatively says the car is asleep. Commanding a sleeping
		// car fails, and a failed resume also never reaches the wake gates; reporting
		// SkipNotCharging instead is what puts waking on the table.
		if vehicle.Session == SessionStoppedForSun && state.IsPluggedIn() {
			if knownAsleep {
				return skip(ActionSkipNotCharging, fmt.Sprintf(
					"%s would resume at %dA but is asleep; only a wake can reach it.", vehicle.VIN, target))
			}
			return Decision{Action: ActionResumeCharging, TargetAmps: intPtr(target), Reason: fmt.Sprintf(
				"Solar recovered to %.2fA; resuming %s at %dA.", amps, vehicle.VIN, target)}
		}

		// Starting a session nobody asked us to start is opt-in, and requires the car to be
		// demonstrably online — not merely to have been plugged in when it last said anything.
		//
		// Learned the hard way: a car plugged in and idle for an hour had gone to sleep, the
		// stored state still said "plugged in", and the command came back "vehicle is offline or
		// asleep". Data age cannot answer this, because a connected parked car sends nothing.
		if opts.StartWhenPluggedIn && state.IsPluggedIn() && vehicle.Online != nil && !*vehicle.Online {
			return skip(ActionSkipNotCharging, fmt.Sprintf(
				"%s is plugged in with %.2fA available but is offline; not waking it.", vehicle.VIN, amps))
		}
		if opts.StartWhenPluggedIn && state.IsPluggedIn() && vehicle.Online != nil && *vehicle.Online {
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
