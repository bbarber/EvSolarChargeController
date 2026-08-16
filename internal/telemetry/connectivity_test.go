package telemetry

import (
	"testing"
	"time"

	"github.com/teslamotors/fleet-telemetry/protos"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func connBytes(t *testing.T, status protos.ConnectivityEvent, vin string, at time.Time) []byte {
	t.Helper()
	raw, err := proto.Marshal(&protos.VehicleConnectivity{
		Vin: vin, Status: status, CreatedAt: timestamppb.New(at), NetworkInterface: "wifi",
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return raw
}

func TestDecodesConnectedAndDisconnected(t *testing.T) {
	cases := map[protos.ConnectivityEvent]bool{
		protos.ConnectivityEvent_CONNECTED:    true,
		protos.ConnectivityEvent_DISCONNECTED: false,
	}

	for status, want := range cases {
		got, err := DecodeConnectivity(connBytes(t, status, testVIN, testNow), time.Now())
		if err != nil {
			t.Fatalf("%v: %v", status, err)
		}
		if got.Online != want {
			t.Errorf("%v gave Online=%v, want %v", status, got.Online, want)
		}
		if got.VIN != testVIN {
			t.Errorf("VIN = %q, want %q", got.VIN, testVIN)
		}
		if !got.ObservedAt.Equal(testNow) {
			t.Errorf("ObservedAt = %v, want %v", got.ObservedAt, testNow)
		}
		if got.Network != "wifi" {
			t.Errorf("Network = %q, want wifi", got.Network)
		}
	}
}

// UNKNOWN must not read as offline. Absence of information is not evidence the car is unreachable,
// and treating it as such would block charging on a single odd frame.
func TestUnknownStatusIsAnErrorNotOffline(t *testing.T) {
	_, err := DecodeConnectivity(connBytes(t, protos.ConnectivityEvent_UNKNOWN, testVIN, testNow), time.Now())
	if err == nil {
		t.Error("expected an error for an UNKNOWN status")
	}
}

func TestConnectivityRejectsAPayloadWithoutAVin(t *testing.T) {
	raw, _ := proto.Marshal(&protos.VehicleConnectivity{Status: protos.ConnectivityEvent_CONNECTED})
	if _, err := DecodeConnectivity(raw, time.Now()); err == nil {
		t.Error("expected an error for a record with no VIN")
	}
}

func TestConnectivityRejectsGarbage(t *testing.T) {
	if _, err := DecodeConnectivity([]byte{0xff, 0xff, 0xff}, time.Now()); err == nil {
		t.Error("expected an error for a malformed record")
	}
}

func TestRecordTypeFromTopic(t *testing.T) {
	cases := map[string]string{
		"evsolar_V":            RecordVehicleData,
		"evsolar_connectivity": RecordConnectivity,
		"evsolar_alerts":       "alerts",
		"V":                    RecordVehicleData,
	}

	for topic, want := range cases {
		if got := RecordTypeFromTopic(topic); got != want {
			t.Errorf("RecordTypeFromTopic(%q) = %q, want %q", topic, got, want)
		}
	}
}
