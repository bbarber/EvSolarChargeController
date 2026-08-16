package telemetry

import (
	"fmt"
	"strings"
	"time"

	"github.com/teslamotors/fleet-telemetry/protos"
	"google.golang.org/protobuf/proto"
)

// Record types fleet-telemetry publishes, as the suffix of "<namespace>_<recordType>".
const (
	RecordVehicleData  = "V"
	RecordConnectivity = "connectivity"
)

// RecordTypeFromTopic strips the namespace from a ZMQ topic frame.
//
// Subscribing to the bare namespace prefix and routing here is what lets one socket carry both
// data and connectivity without a second subscriber.
func RecordTypeFromTopic(topic string) string {
	if i := strings.LastIndex(topic, "_"); i >= 0 {
		return topic[i+1:]
	}
	return topic
}

// Connectivity is a vehicle connection state change.
//
// Tesla's README describes these events as a proxy for vehicle online state, matching real
// connectivity better than 99% of the time. That is a far better signal than inferring liveness
// from how long ago the last data frame arrived: a connected car that is simply parked and
// unchanging sends no data at all, and a car that has fallen asleep looks identical.
type Connectivity struct {
	VIN        string
	Online     bool
	ObservedAt time.Time
	Network    string
}

// DecodeConnectivity unmarshals a connectivity record.
//
// An UNKNOWN status is reported as an error rather than as offline: absence of information must
// not read as evidence the car is unreachable, or a single odd frame would block charging.
func DecodeConnectivity(raw []byte, fallbackNow time.Time) (Connectivity, error) {
	var msg protos.VehicleConnectivity
	if err := proto.Unmarshal(raw, &msg); err != nil {
		return Connectivity{}, fmt.Errorf("unmarshalling connectivity record: %w", err)
	}
	if msg.Vin == "" {
		return Connectivity{}, fmt.Errorf("connectivity record carried no VIN")
	}

	c := Connectivity{VIN: msg.Vin, ObservedAt: fallbackNow, Network: msg.NetworkInterface}
	if ts := msg.CreatedAt; ts != nil && ts.IsValid() {
		c.ObservedAt = ts.AsTime().UTC()
	}

	switch msg.Status {
	case protos.ConnectivityEvent_CONNECTED:
		c.Online = true
	case protos.ConnectivityEvent_DISCONNECTED:
		c.Online = false
	default:
		return Connectivity{}, fmt.Errorf("connectivity record for %s had an unknown status", msg.Vin)
	}

	return c, nil
}
