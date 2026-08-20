package domain

import (
	"strings"
	"time"
)

// ChargingState is the normalised charge state, mapped from Tesla's DetailedChargeState telemetry
// field (179). The proto also defines a ChargingState enum, but Tesla's schema marks it
// "deprecated and not used", so DetailedChargeState is authoritative.
type ChargingState int

const (
	StateUnknown ChargingState = iota
	StateDisconnected
	StateNoPower
	StateStarting
	StateCharging
	StateComplete
	StateStopped
)

var chargingStateNames = map[ChargingState]string{
	StateUnknown:      "Unknown",
	StateDisconnected: "Disconnected",
	StateNoPower:      "NoPower",
	StateStarting:     "Starting",
	StateCharging:     "Charging",
	StateComplete:     "Complete",
	StateStopped:      "Stopped",
}

func (s ChargingState) String() string {
	if name, ok := chargingStateNames[s]; ok {
		return name
	}
	return "Unknown"
}

// ParseChargingState maps a stored or telemetry-supplied name onto the enum, falling back to
// Unknown. Tesla has shipped these as differing cases across firmware versions.
func ParseChargingState(value string) ChargingState {
	for state, name := range chargingStateNames {
		if strings.EqualFold(name, value) {
			return state
		}
	}
	return StateUnknown
}

// IsActivelyCharging reports whether the car is drawing current and will honour an amp change.
func (s ChargingState) IsActivelyCharging() bool { return s == StateCharging }

// IsUnplugged reports whether the connector is out of the car. This is the event that clears a
// manual override — an override persists until the vehicle unplugs, not merely until it pauses.
func (s ChargingState) IsUnplugged() bool { return s == StateDisconnected }

// IsPluggedIn reports whether the cable is still attached, even if not drawing power.
func (s ChargingState) IsPluggedIn() bool {
	switch s {
	case StateCharging, StateComplete, StateStarting, StateStopped, StateNoPower:
		return true
	default:
		return false
	}
}

// VehicleState is the latest known charge state for one vehicle plus what this controller last
// commanded. One record per VIN.
type VehicleState struct {
	VIN string

	// ChargeAmps is the charge current the vehicle currently reports.
	ChargeAmps *int

	// ReportedMaxAmps is the vehicle-reported ceiling, when the car sends it. The car may cap
	// below our configured maximum for breaker or on-board-charger reasons.
	ReportedMaxAmps *int

	BatteryLevelPercent *int

	ChargingState ChargingState

	// Session is this controller's relationship to the charge session; SessionSince is when it
	// entered that state. Since is what the settle window is measured against: telemetry emitted
	// before our own stop landed still says Charging, and must not read as a manual restart.
	Session      SessionState
	SessionSince *time.Time

	// LastSetAmps is the last value this controller successfully commanded, or nil if none.
	LastSetAmps *int
	LastSetAt   *time.Time

	LastUpdated time.Time

	// Online is the vehicle's connection state as reported by fleet-telemetry's connectivity
	// stream. Nil means never observed — which is treated as "do not assume reachable".
	//
	// This is not the same question as whether telemetry is recent. A parked, connected car sends
	// no data for hours because signals transmit on change, so data age says nothing about
	// reachability; a connectivity event does.
	Online   *bool
	OnlineAt *time.Time

	// LastWakeAt is when this controller last woke the car, for the cooldown.
	LastWakeAt *time.Time

	// AtHome is whether the car's last reported position was within the home radius. Nil until a
	// location frame has been seen. Only this boolean is stored — never the position itself.
	//
	// AtHomeAt is when that determination was made. Without it a stale answer is indistinguishable
	// from a current one, and "away" latched from this morning's drive silently governs a car that
	// has been sitting on the home connector for hours.
	AtHome   *bool
	AtHomeAt *time.Time

	// FastCharger is whether a DC fast charger is attached, from FastChargerPresent. A fast-charge
	// session is never touched, wherever it is.
	FastCharger *bool
}

func NewVehicleState(vin string, now time.Time) *VehicleState {
	return &VehicleState{
		VIN:           vin,
		ChargingState: StateUnknown,
		LastUpdated:   now,
	}
}

func intPtr(v int) *int              { return &v }
func timePtr(v time.Time) *time.Time { return &v }
