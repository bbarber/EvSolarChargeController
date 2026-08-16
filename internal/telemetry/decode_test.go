package telemetry

import (
	"testing"
	"time"

	"github.com/teslamotors/fleet-telemetry/protos"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/bbarber/EvSolarChargeController/internal/domain"
)

var (
	testVIN = "5YJ3E1EA3KF428848"
	testNow = time.Date(2026, 8, 11, 14, 0, 0, 0, time.UTC)
)

func datum(key protos.Field, value *protos.Value) *protos.Datum {
	return &protos.Datum{Key: key, Value: value}
}

func intValue(v int32) *protos.Value {
	return &protos.Value{Value: &protos.Value_IntValue{IntValue: v}}
}

func payloadWith(data ...*protos.Datum) *protos.Payload {
	return &protos.Payload{Vin: testVIN, CreatedAt: timestamppb.New(testNow), Data: data}
}

func TestDecodesTheChargingFields(t *testing.T) {
	p := payloadWith(
		datum(protos.Field_ChargeAmps, intValue(12)),
		datum(protos.Field_ChargeCurrentRequestMax, intValue(16)),
		datum(protos.Field_BatteryLevel, intValue(64)),
		datum(protos.Field_DetailedChargeState, &protos.Value{
			Value: &protos.Value_DetailedChargeStateValue{
				DetailedChargeStateValue: protos.DetailedChargeStateValue_DetailedChargeStateCharging,
			},
		}),
	)

	obs, err := Decode(p, time.Now())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if obs.VIN != testVIN {
		t.Errorf("VIN = %q, want %q", obs.VIN, testVIN)
	}
	if !obs.ObservedAt.Equal(testNow) {
		t.Errorf("ObservedAt = %v, want %v", obs.ObservedAt, testNow)
	}
	if obs.ReportedAmps == nil || *obs.ReportedAmps != 12 {
		t.Errorf("ReportedAmps = %v, want 12", obs.ReportedAmps)
	}
	if obs.ReportedMaxAmps == nil || *obs.ReportedMaxAmps != 16 {
		t.Errorf("ReportedMaxAmps = %v, want 16", obs.ReportedMaxAmps)
	}
	if obs.BatteryLevelPercent == nil || *obs.BatteryLevelPercent != 64 {
		t.Errorf("BatteryLevelPercent = %v, want 64", obs.BatteryLevelPercent)
	}
	if obs.ChargingState == nil || *obs.ChargingState != domain.StateCharging {
		t.Errorf("ChargingState = %v, want Charging", obs.ChargingState)
	}
}

func TestAcceptsEveryNumericVariant(t *testing.T) {
	// Tesla has shipped these as int, long, float, double and string across firmware versions.
	cases := []struct {
		name  string
		value *protos.Value
		want  int
	}{
		{"int", intValue(12), 12},
		{"long", &protos.Value{Value: &protos.Value_LongValue{LongValue: 12}}, 12},
		{"float", &protos.Value{Value: &protos.Value_FloatValue{FloatValue: 11.5}}, 12},
		{"double", &protos.Value{Value: &protos.Value_DoubleValue{DoubleValue: 11.4}}, 11},
		{"string", &protos.Value{Value: &protos.Value_StringValue{StringValue: "12"}}, 12},
		{"string with decimal", &protos.Value{Value: &protos.Value_StringValue{StringValue: "11.6"}}, 12},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obs, err := Decode(payloadWith(datum(protos.Field_ChargeAmps, c.value)), testNow)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if obs.ReportedAmps == nil || *obs.ReportedAmps != c.want {
				t.Errorf("ReportedAmps = %v, want %d", obs.ReportedAmps, c.want)
			}
		})
	}
}

func TestUnparseableStringIsIgnoredRatherThanZero(t *testing.T) {
	// Zero would read as "the user set 0A", which trips override detection.
	obs, err := Decode(payloadWith(datum(protos.Field_ChargeAmps,
		&protos.Value{Value: &protos.Value_StringValue{StringValue: "<invalid>"}})), testNow)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if obs.ReportedAmps != nil {
		t.Errorf("ReportedAmps = %v, want nil", obs.ReportedAmps)
	}
}

func TestChargeCurrentRequestIsOnlyAFallback(t *testing.T) {
	both, err := Decode(payloadWith(
		datum(protos.Field_ChargeAmps, intValue(12)),
		datum(protos.Field_ChargeCurrentRequest, intValue(32)),
	), testNow)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if both.ReportedAmps == nil || *both.ReportedAmps != 12 {
		t.Errorf("with both present, ReportedAmps = %v, want ChargeAmps 12", both.ReportedAmps)
	}

	only, err := Decode(payloadWith(datum(protos.Field_ChargeCurrentRequest, intValue(32))), testNow)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if only.ReportedAmps == nil || *only.ReportedAmps != 32 {
		t.Errorf("with only the fallback, ReportedAmps = %v, want 32", only.ReportedAmps)
	}
}

func TestAllDetailedChargeStatesMap(t *testing.T) {
	cases := map[protos.DetailedChargeStateValue]domain.ChargingState{
		protos.DetailedChargeStateValue_DetailedChargeStateUnknown:      domain.StateUnknown,
		protos.DetailedChargeStateValue_DetailedChargeStateDisconnected: domain.StateDisconnected,
		protos.DetailedChargeStateValue_DetailedChargeStateNoPower:      domain.StateNoPower,
		protos.DetailedChargeStateValue_DetailedChargeStateStarting:     domain.StateStarting,
		protos.DetailedChargeStateValue_DetailedChargeStateCharging:     domain.StateCharging,
		protos.DetailedChargeStateValue_DetailedChargeStateComplete:     domain.StateComplete,
		protos.DetailedChargeStateValue_DetailedChargeStateStopped:      domain.StateStopped,
	}

	for wire, want := range cases {
		obs, err := Decode(payloadWith(datum(protos.Field_DetailedChargeState,
			&protos.Value{Value: &protos.Value_DetailedChargeStateValue{DetailedChargeStateValue: wire}})), testNow)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if obs.ChargingState == nil || *obs.ChargingState != want {
			t.Errorf("%v mapped to %v, want %v", wire, obs.ChargingState, want)
		}
	}
}

func TestOnlyAnExplicitDisengagedLatchCountsAsUnplugged(t *testing.T) {
	// Unknown and SNA are absence of information. Treating them as an unplug would clear a manual
	// override early and hand control back while the cable is still in.
	cases := map[protos.ChargePortLatchValue]bool{
		protos.ChargePortLatchValue_ChargePortLatchDisengaged: true,
		protos.ChargePortLatchValue_ChargePortLatchEngaged:    false,
		protos.ChargePortLatchValue_ChargePortLatchUnknown:    false,
		protos.ChargePortLatchValue_ChargePortLatchSNA:        false,
		protos.ChargePortLatchValue_ChargePortLatchBlocking:   false,
	}

	for wire, want := range cases {
		obs, err := Decode(payloadWith(datum(protos.Field_ChargePortLatch,
			&protos.Value{Value: &protos.Value_ChargePortLatchValue{ChargePortLatchValue: wire}})), testNow)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if obs.LatchDisengaged == nil || *obs.LatchDisengaged != want {
			t.Errorf("%v gave LatchDisengaged=%v, want %v", wire, obs.LatchDisengaged, want)
		}
	}
}

func TestAbsentFieldsStayNil(t *testing.T) {
	// This is what lets the override rule preserve previous values on a partial frame.
	obs, err := Decode(payloadWith(datum(protos.Field_ChargeAmps, intValue(12))), testNow)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if obs.ReportedMaxAmps != nil || obs.BatteryLevelPercent != nil ||
		obs.ChargingState != nil || obs.LatchDisengaged != nil {
		t.Errorf("expected untouched fields to stay nil, got %+v", obs)
	}
}

func TestFallsBackToNowWhenCreatedAtIsAbsent(t *testing.T) {
	obs, err := Decode(&protos.Payload{Vin: testVIN}, testNow)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !obs.ObservedAt.Equal(testNow) {
		t.Errorf("ObservedAt = %v, want the fallback %v", obs.ObservedAt, testNow)
	}
}

func TestRejectsAPayloadWithoutAVin(t *testing.T) {
	// Without a VIN there is no record to fold the observation into.
	if _, err := Decode(&protos.Payload{}, testNow); err == nil {
		t.Error("expected an error for a payload with no VIN")
	}
}

func TestDecodeBytesRoundTripsTheWireFormat(t *testing.T) {
	raw, err := proto.Marshal(payloadWith(datum(protos.Field_ChargeAmps, intValue(9))))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	obs, err := DecodeBytes(raw, testNow)
	if err != nil {
		t.Fatalf("DecodeBytes: %v", err)
	}
	if obs.ReportedAmps == nil || *obs.ReportedAmps != 9 {
		t.Errorf("ReportedAmps = %v, want 9", obs.ReportedAmps)
	}
}

func TestDecodeBytesRejectsGarbage(t *testing.T) {
	if _, err := DecodeBytes([]byte{0xff, 0xff, 0xff, 0xff}, testNow); err == nil {
		t.Error("expected an error for a malformed payload")
	}
}
