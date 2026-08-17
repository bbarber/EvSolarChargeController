package domain

import "strings"

// SessionState is this controller's relationship to the charge session — distinct from
// ChargingState, which is what the car reports about itself.
//
// This used to be encoded in three nullable markers (OverrideActive, SocStopIssuedAt,
// LowSolarStopIssuedAt) with implicit precedence between them. Every subtle bug in the project's
// history involved marker interactions: a stale marker making our own resume look like a manual
// restart, a stop marker surviving into the next session, precedence readable only by tracing the
// code. One named state with named transitions makes those interactions inspectable.
type SessionState int

const (
	// SessionAuto is normal automatic control: nothing stopped, nothing overridden. The zero
	// value on purpose — a fresh vehicle record starts here.
	SessionAuto SessionState = iota

	// SessionStoppedForSun means this controller stopped the session because production could not
	// sustain the connector minimum (or the sun set). Resumes automatically when the sun returns.
	SessionStoppedForSun

	// SessionStoppedAtCap means this controller stopped at the state-of-charge cap. Does not
	// resume automatically; a person restarting it is treated as an override.
	SessionStoppedAtCap

	// SessionOverridden means a human took control — changed amps in the app, or restarted a
	// session we stopped. Automatic control stays hands-off until the car unplugs.
	SessionOverridden
)

var sessionNames = map[SessionState]string{
	SessionAuto:          "Auto",
	SessionStoppedForSun: "StoppedForSun",
	SessionStoppedAtCap:  "StoppedAtCap",
	SessionOverridden:    "Overridden",
}

func (s SessionState) String() string {
	if name, ok := sessionNames[s]; ok {
		return name
	}
	return "Auto"
}

// ParseSessionState maps a stored name back onto the enum, defaulting to Auto — the safe state,
// since Auto grants the controller no special permissions it has to earn.
func ParseSessionState(value string) SessionState {
	for state, name := range sessionNames {
		if strings.EqualFold(name, value) {
			return state
		}
	}
	return SessionAuto
}

// StoppedByUs reports whether this controller ended the current session, for either reason.
// A charging car in one of these states means a person restarted it by hand.
func (s SessionState) StoppedByUs() bool {
	return s == SessionStoppedForSun || s == SessionStoppedAtCap
}
