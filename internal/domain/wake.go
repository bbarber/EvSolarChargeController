package domain

import (
	"fmt"
	"math"
	"time"
)

// WakeDecision is whether to wake a sleeping car so it can use available sun, and why.
//
// Kept separate from Decide because it answers a different question. Decide asks "what current
// should this car draw"; this asks "is it worth spending $0.02 and a battery-draining wake window
// to find out". Every gate below has a reason to exist, and the refusals are as interesting as the
// approvals — the Reason is logged either way.
type WakeDecision struct {
	Wake   bool
	Reason string
}

func noWake(format string, args ...any) WakeDecision {
	return WakeDecision{Reason: fmt.Sprintf(format, args...)}
}

// WakeInputs is what the controller has already measured this cycle.
type WakeInputs struct {
	// MaxAmpsLastWindow is the trailing maximum, nil when the window holds no readings.
	MaxAmpsLastWindow *float64

	// ReadingsAboveMinimum counts readings in the trailing window that could sustain a session.
	// Requiring more than one is what stops a single sunbreak triggering a wake.
	ReadingsAboveMinimum int

	// WakesToday is how many times this car has already been woken today.
	WakesToday int

	// LocalNow is wall-clock time in the configured zone. The day restriction is a human
	// preference about which days it is acceptable to be woken, so it must be read locally — UTC
	// would shift the boundary by hours and change which days qualify.
	LocalNow time.Time
}

// DecideWake applies every gate in order of how cheap it is to check.
func DecideWake(vehicle *VehicleState, in WakeInputs, opts ChargingOptions) WakeDecision {
	if !opts.WakeToCharge {
		return noWake("Waking is disabled.")
	}
	if vehicle == nil {
		return noWake("No vehicle state.")
	}

	if !dayAllowed(in.LocalNow.Weekday(), opts.WakeDays) {
		return noWake("%s is not a permitted wake day (%s).",
			in.LocalNow.Weekday(), describeDays(opts.WakeDays))
	}

	if in.WakesToday >= opts.MaxWakesPerDay {
		return noWake("Already woken %d times today (limit %d).", in.WakesToday, opts.MaxWakesPerDay)
	}

	if vehicle.LastWakeAt != nil {
		if since := in.LocalNow.Sub(*vehicle.LastWakeAt); since < opts.WakeCooldown {
			return noWake("Last wake was %s ago, inside the %s cooldown.",
				since.Round(time.Minute), opts.WakeCooldown)
		}
	}

	// Only wake something known to be asleep. Online means it is reachable already, and unknown
	// means we have never had a connectivity event and should not be guessing.
	if vehicle.Online == nil {
		return noWake("Connectivity has never been observed for %s.", vehicle.VIN)
	}
	if *vehicle.Online {
		return noWake("%s is already online.", vehicle.VIN)
	}

	if vehicle.Session == SessionOverridden {
		return noWake("Manual override active for %s.", vehicle.VIN)
	}

	// The car cannot tell us it is plugged in while it is asleep, so this is the last thing it
	// said before dozing off. Unplugging wakes a sleeping car, so a disconnect would normally have
	// reached us first — but this is an inference, not an observation, and the wake is wasted if
	// it is wrong.
	if !vehicle.ChargingState.IsPluggedIn() {
		return noWake("%s was last seen %s, not plugged in.", vehicle.VIN, vehicle.ChargingState)
	}

	// Never wake a car that is not known to be at home. Plugged in somewhere else means someone
	// else's charger, and the wake would at best be wasted.
	if opts.HomeConfigured() {
		if vehicle.AtHome == nil {
			return noWake("%s has never reported a position; not waking on a guess.", vehicle.VIN)
		}
		if !*vehicle.AtHome {
			return noWake("%s was last seen away from home.", vehicle.VIN)
		}
	}

	soc := vehicle.BatteryLevelPercent
	if soc == nil {
		return noWake("State of charge unknown for %s.", vehicle.VIN)
	}
	if ceiling := opts.MaxSocPercent - opts.WakeSocHeadroom; *soc >= ceiling {
		return noWake("%s is at %d%%, within %d points of the %d%% cap; not worth a wake.",
			vehicle.VIN, *soc, opts.WakeSocHeadroom, opts.MaxSocPercent)
	}

	if in.MaxAmpsLastWindow == nil {
		return noWake("No solar readings in the trailing window.")
	}
	amps := *in.MaxAmpsLastWindow
	if int(math.Round(amps)) < opts.MinChargeAmps {
		return noWake("Solar peaked at %.2fA, below the %dA minimum.", amps, opts.MinChargeAmps)
	}
	if in.ReadingsAboveMinimum < 2 {
		return noWake("Only %d reading above the minimum; waiting for a sustained window.",
			in.ReadingsAboveMinimum)
	}

	return WakeDecision{Wake: true, Reason: fmt.Sprintf(
		"%s is asleep and plugged in at %d%%, with %.2fA sustained; waking to charge.",
		vehicle.VIN, *soc, amps)}
}

func dayAllowed(day time.Weekday, allowed []time.Weekday) bool {
	if len(allowed) == 0 {
		return true // Unrestricted.
	}
	for _, d := range allowed {
		if d == day {
			return true
		}
	}
	return false
}

func describeDays(days []time.Weekday) string {
	if len(days) == 0 {
		return "any day"
	}
	out := ""
	for i, d := range days {
		if i > 0 {
			out += ", "
		}
		out += d.String()[:3]
	}
	return out
}
