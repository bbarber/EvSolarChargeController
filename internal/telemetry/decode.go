// Package telemetry turns fleet-telemetry records into domain observations.
//
// The protobuf types come straight from github.com/teslamotors/fleet-telemetry/protos, so there is
// no vendored .proto file and no code generation step — the schema tracks Tesla's releases through
// an ordinary module upgrade.
package telemetry

import (
	"fmt"
	"strconv"
	"time"

	"github.com/teslamotors/fleet-telemetry/protos"
	"google.golang.org/protobuf/proto"

	"github.com/bbarber/EvSolarChargeController/internal/domain"
)

// Decode folds one payload into an observation.
//
// Every field is optional. Tesla streams signals on change rather than sending a full snapshot, so
// a frame carrying only ChargeAmps must leave every other field nil for the override rule to
// preserve the previous values.
func Decode(payload *protos.Payload, fallbackNow time.Time) (domain.Observation, error) {
	if payload == nil {
		return domain.Observation{}, fmt.Errorf("nil payload")
	}
	if payload.Vin == "" {
		return domain.Observation{}, fmt.Errorf("payload carried no VIN")
	}

	obs := domain.Observation{VIN: payload.Vin, ObservedAt: fallbackNow}
	if ts := payload.CreatedAt; ts != nil && ts.IsValid() {
		obs.ObservedAt = ts.AsTime().UTC()
	}

	// ChargeAmps is preferred and ChargeCurrentRequest is the fallback, matching the previous
	// implementation. Which one actually tracks the slider in the Tesla app has never been
	// confirmed against a real vehicle, and it is the most likely source of false override
	// detection in the field.
	var chargeCurrentRequest *int

	for _, datum := range payload.Data {
		if datum == nil || datum.Value == nil {
			continue
		}

		switch datum.Key {
		case protos.Field_ChargeAmps:
			if v, ok := numericValue(datum.Value); ok {
				obs.ReportedAmps = intPtr(v)
			}

		case protos.Field_ChargeCurrentRequest:
			if v, ok := numericValue(datum.Value); ok {
				chargeCurrentRequest = intPtr(v)
			}

		case protos.Field_ChargeCurrentRequestMax:
			if v, ok := numericValue(datum.Value); ok {
				obs.ReportedMaxAmps = intPtr(v)
			}

		case protos.Field_BatteryLevel, protos.Field_Soc:
			if v, ok := numericValue(datum.Value); ok {
				obs.BatteryLevelPercent = intPtr(v)
			}

		case protos.Field_DetailedChargeState:
			if state, ok := chargingState(datum.Value); ok {
				obs.ChargingState = &state
			}

		case protos.Field_ChargePortLatch:
			if latch, ok := datum.Value.Value.(*protos.Value_ChargePortLatchValue); ok {
				// Only an explicit Disengaged clears an override. Unknown and SNA are absence of
				// information, and treating them as an unplug would hand control back too early.
				disengaged := latch.ChargePortLatchValue == protos.ChargePortLatchValue_ChargePortLatchDisengaged
				obs.LatchDisengaged = &disengaged
			}
		}
	}

	if obs.ReportedAmps == nil && chargeCurrentRequest != nil {
		obs.ReportedAmps = chargeCurrentRequest
	}

	return obs, nil
}

// DecodeBytes unmarshals a wire record and decodes it.
func DecodeBytes(raw []byte, fallbackNow time.Time) (domain.Observation, error) {
	var payload protos.Payload
	if err := proto.Unmarshal(raw, &payload); err != nil {
		return domain.Observation{}, fmt.Errorf("unmarshalling telemetry payload: %w", err)
	}
	return Decode(&payload, fallbackNow)
}

// numericValue accepts whichever numeric variant the firmware happened to send. Tesla has shipped
// these fields as int, long, float, double and even string across versions, so pinning to one
// variant would silently drop data after a car update.
func numericValue(v *protos.Value) (int, bool) {
	switch typed := v.Value.(type) {
	case *protos.Value_IntValue:
		return int(typed.IntValue), true
	case *protos.Value_LongValue:
		return int(typed.LongValue), true
	case *protos.Value_FloatValue:
		return int(roundHalfAwayFromZero(float64(typed.FloatValue))), true
	case *protos.Value_DoubleValue:
		return int(roundHalfAwayFromZero(typed.DoubleValue)), true
	case *protos.Value_StringValue:
		if parsed, err := strconv.ParseFloat(typed.StringValue, 64); err == nil {
			return int(roundHalfAwayFromZero(parsed)), true
		}
	}
	return 0, false
}

func chargingState(v *protos.Value) (domain.ChargingState, bool) {
	typed, ok := v.Value.(*protos.Value_DetailedChargeStateValue)
	if !ok {
		return domain.StateUnknown, false
	}

	// The enum values line up one-for-one with the domain states, but they are mapped explicitly
	// rather than cast, so a renumbering upstream becomes a compile error instead of silent
	// mis-detection of whether the car is charging.
	switch typed.DetailedChargeStateValue {
	case protos.DetailedChargeStateValue_DetailedChargeStateDisconnected:
		return domain.StateDisconnected, true
	case protos.DetailedChargeStateValue_DetailedChargeStateNoPower:
		return domain.StateNoPower, true
	case protos.DetailedChargeStateValue_DetailedChargeStateStarting:
		return domain.StateStarting, true
	case protos.DetailedChargeStateValue_DetailedChargeStateCharging:
		return domain.StateCharging, true
	case protos.DetailedChargeStateValue_DetailedChargeStateComplete:
		return domain.StateComplete, true
	case protos.DetailedChargeStateValue_DetailedChargeStateStopped:
		return domain.StateStopped, true
	case protos.DetailedChargeStateValue_DetailedChargeStateUnknown:
		return domain.StateUnknown, true
	}
	return domain.StateUnknown, false
}

func roundHalfAwayFromZero(v float64) float64 {
	if v < 0 {
		return -roundHalfAwayFromZero(-v)
	}
	return float64(int(v + 0.5))
}

func intPtr(v int) *int { return &v }
