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
	//
	// It was 60 minutes, which turned out to over-damp: the window could not tell a 15-minute
	// cloud from a three-hour sunset, so the target never decayed and the car kept drawing its
	// early-afternoon current into the evening on grid power. At 20 minutes a passing cloud is
	// still inside the window — the max holds through it — while a sustained decline is tracked
	// down with about a 20-minute lag. Measured over five simulated days, that cut grid import by
	// roughly 70% for two extra commands a day.
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
		LookbackWindow:         20 * time.Minute,
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
