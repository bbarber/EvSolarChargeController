package domain

import "time"

// ChargingOptions holds the electrical and charging limits used to turn solar production into a
// charge current.
type ChargingOptions struct {
	// SystemVoltage is the divisor for the watts -> amps conversion. US residential split-phase
	// is 240V; every target current scales directly with this.
	SystemVoltage float64

	// MinChargeAmps is the lowest current the wall connector will accept. Production below this
	// stops the session rather than clamping up, because clamping up draws the shortfall from
	// the grid.
	MinChargeAmps int

	// MaxChargeAmps is the ceiling we will ever request. Matched to the array's peak output;
	// asking for more would only ever pull the difference from the grid.
	MaxChargeAmps int

	// LookbackWindow is the trailing window the "max amps seen recently" figure is taken over.
	// Taking the maximum biases toward overshoot rather than chasing every passing cloud.
	LookbackWindow time.Duration

	// MaxSocPercent is a cap this controller will not drive charging past, regardless of the
	// limit set on the vehicle. A person can still charge past it by hand, which is treated as
	// a manual override.
	MaxSocPercent int

	// OverrideSettleWindow is how long reported-amps mismatches are ignored after we issue a
	// command, so in-flight telemetry carrying the previous value is not misread as a person
	// moving the slider.
	OverrideSettleWindow time.Duration

	// VehicleStateStaleAfter is the age past which telemetry is treated as unknown rather than
	// authoritative — the car is probably asleep.
	VehicleStateStaleAfter time.Duration
}

// DefaultChargingOptions holds the tuned values; every one is overridable from the environment.
func DefaultChargingOptions() ChargingOptions {
	return ChargingOptions{
		SystemVoltage:          240,
		MinChargeAmps:          5,
		MaxChargeAmps:          16,
		LookbackWindow:         60 * time.Minute,
		MaxSocPercent:          80,
		OverrideSettleWindow:   3 * time.Minute,
		VehicleStateStaleAfter: 6 * time.Hour,
	}
}

// PollingWindowOptions bounds the daylight hours in which solar is polled at all.
type PollingWindowOptions struct {
	// TimeZone is an IANA identifier such as America/Chicago.
	TimeZone string

	StartHourLocal int

	// EndHourLocal is exclusive. 18 means the last poll is at 17:40 local.
	EndHourLocal int
}

func DefaultPollingWindowOptions() PollingWindowOptions {
	return PollingWindowOptions{
		TimeZone:       "America/Chicago",
		StartHourLocal: 9,
		EndHourLocal:   18,
	}
}
