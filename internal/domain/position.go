package domain

import (
	"fmt"
	"time"
)

// PositionDecision is whether to spend one Fleet API read resolving where a car is, and why.
//
// This is deliberately its own question, like DecideWake. The home gate can only skip when it does
// not know where the car is, and a skip is indistinguishable from a car that is genuinely away —
// so the only way out of a stale "away" is to ask.
type PositionDecision struct {
	Resolve bool
	Reason  string
}

func noResolve(format string, args ...any) PositionDecision {
	return PositionDecision{Reason: fmt.Sprintf(format, args...)}
}

// DecidePositionFix applies every gate in order of how cheap it is to check.
//
// The "never poll the vehicle" invariant is about not *waking* a car: routine collection must never
// disturb a vehicle parked far from a charger. This asks only about a car that connectivity has
// just reported online and that is plugged in, so the read cannot wake anything — and it happens
// once per plug-in rather than on a timer.
func DecidePositionFix(state *VehicleState, lastAttempt *time.Time, opts ChargingOptions, now time.Time) PositionDecision {
	if !opts.HomeConfigured() {
		return noResolve("The home gate is off; position is not consulted.")
	}
	if state == nil {
		return noResolve("No vehicle state.")
	}

	// Only a car that is already awake. Online is the reachability truth; data age is not.
	if state.Online == nil || !*state.Online {
		return noResolve("%s is not known to be online; a read could wake it.", state.VIN)
	}

	// Only a car at a connector. Position does not matter for an unplugged car, and asking about
	// one is the routine collection the invariant forbids.
	if !state.ChargingState.IsPluggedIn() {
		return noResolve("%s is %s, not plugged in.", state.VIN, state.ChargingState)
	}

	if state.FastCharger != nil && *state.FastCharger {
		return noResolve("%s is on a DC fast charger, which is never managed.", state.VIN)
	}

	if lastAttempt != nil {
		if since := now.Sub(*lastAttempt); since < opts.PositionRefreshCooldown {
			return noResolve("Position was read %s ago, inside the %s cooldown.",
				since.Round(time.Second), opts.PositionRefreshCooldown)
		}
	}

	if state.AtHome == nil {
		return PositionDecision{Resolve: true, Reason: fmt.Sprintf(
			"%s has never reported a position and is plugged in; resolving it.", state.VIN)}
	}
	if state.AtHomeAt == nil {
		return PositionDecision{Resolve: true, Reason: fmt.Sprintf(
			"%s has a position of unknown age; resolving it.", state.VIN)}
	}
	if age := now.Sub(*state.AtHomeAt); age >= opts.PositionMaxAge {
		return PositionDecision{Resolve: true, Reason: fmt.Sprintf(
			"%s position is %s old, past the %s limit; resolving it.",
			state.VIN, age.Round(time.Minute), opts.PositionMaxAge)}
	}

	return noResolve("%s position is current.", state.VIN)
}
