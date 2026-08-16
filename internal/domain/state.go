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

	// SocStopIssuedAt records when this controller last stopped charging because the state-of-
	// charge cap was reached. If the car is charging again well after this, someone restarted it.
	SocStopIssuedAt *time.Time

	// LowSolarStopIssuedAt records when this controller last stopped charging because solar could
	// not cover the minimum current. Unlike the SoC cap, this one resumes automatically.
	LowSolarStopIssuedAt *time.Time

	ChargingState ChargingState

	OverrideActive     bool
	OverrideDetectedAt *time.Time

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
