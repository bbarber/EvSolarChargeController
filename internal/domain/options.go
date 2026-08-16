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

	// StartWhenPluggedIn lets the controller begin a session on a car that is plugged in and idle
	// but that this controller did not stop — a car you plugged in and left, with the sun out.
	//
	// Off by default, because the general rule is to never command a car that is not already
	// charging: a command can wake a sleeping vehicle. This is the one safe exception, and only
	// because telemetry has just told us the car is plugged in and awake enough to be reporting.
	// It still sits behind every other gate — override, the state-of-charge cap, and production
	// clearing the connector minimum are all checked first.
	StartWhenPluggedIn bool

	// WakeToCharge lets the controller wake a sleeping car to use available sun.
	//
	// Off by default, and the most carefully gated thing here. Waking is not what the "never poll"
	// rule was protecting against — that rule exists so routine data collection does not
	// incidentally wake a car parked far from a charger. A deliberate wake, on a car last seen
	// plugged in, with sun we intend to use, is a different act. It is still rate-limited,
	// day-restricted and counted, because it costs $0.02 a time and drains the battery it is
	// trying to fill.
	WakeToCharge bool

	// WakeDays restricts waking to particular days. Empty means every day.
	WakeDays []time.Weekday

	// MaxWakesPerDay is a hard ceiling. Tesla sizes the $10 monthly discount at roughly two wakes
	// a day for two vehicles, so this is their number rather than an arbitrary one.
	MaxWakesPerDay int

	// WakeCooldown is the minimum gap between wakes, which stops the worst failure mode: wake,
	// car comes up, solar dips before the command lands, car sleeps, wake again.
	WakeCooldown time.Duration

	// WakeSocHeadroom is how far below the cap the battery must be for a wake to be worth it.
	// Waking at 79% spends $0.02 and a battery-draining wake window for a few minutes of charge.
	WakeSocHeadroom int

	// SustainedWindow is how far back the wake gate looks to decide the sun is reliable rather
	// than a sunbreak. It must be longer than the poll interval, or fewer than two readings can
	// ever fall inside it.
	//
	// Deliberately not LookbackWindow. That one wants to be *short* so the target tracks a
	// declining sun; this one wants to be long enough to see a trend. Sharing a value made the
	// wake gate unsatisfiable: pruning at the lookback horizon deleted the previous reading
	// moments before the gate counted, so the window held one reading and the gate wanted two.
	SustainedWindow time.Duration

	// ReadingRetention is how long readings are kept. Only storage hygiene — targeting and the
	// wake gate each take their own window over whatever is retained, so this simply has to be
	// longer than both. Keeping a couple of hours also leaves a real production history in the
	// database rather than only in container logs.
	ReadingRetention time.Duration
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
		WakeDays:               []time.Weekday{time.Friday, time.Saturday, time.Sunday},
		MaxWakesPerDay:         2,
		WakeCooldown:           time.Hour,
		WakeSocHeadroom:        10,
		SustainedWindow:        45 * time.Minute,
		ReadingRetention:       6 * time.Hour,
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
