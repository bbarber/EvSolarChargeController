package controller

import (
	"context"
	"testing"
	"time"

	"github.com/teslamotors/fleet-telemetry/protos"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/bbarber/EvSolarChargeController/internal/domain"
)

// Decisions ride on events; the clock only fetches solar. These tests pin the difference that
// motivated the change: everything observable used to wait up to twenty minutes for a tick, and
// the car does not stay awake that long.

func connectivityFrame(t *testing.T, online bool, at time.Time) (string, []byte) {
	t.Helper()
	status := protos.ConnectivityEvent_DISCONNECTED
	if online {
		status = protos.ConnectivityEvent_CONNECTED
	}
	raw, err := proto.Marshal(&protos.VehicleConnectivity{
		Vin: testVIN, Status: status, CreatedAt: timestamppb.New(at),
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return "evsolar_connectivity", raw
}

// The race that wasted the first wake this system ever sent: wake at :40, car online within
// seconds, asleep again before the :00 tick. The online event itself must now trigger the start,
// while the car is provably awake.
func TestComingOnlineAfterAWakeStartsChargingImmediately(t *testing.T) {
	// A Sunday afternoon inside the window, sun above the floor.
	now := time.Date(2026, 8, 16, 19, 41, 0, 0, time.UTC)

	solar := &fakeSolar{}
	cmd := &fakeCommander{}
	c, st := newController(t, solar, cmd)
	c.now = func() time.Time { return now }

	opts := domain.DefaultChargingOptions()
	opts.StartWhenPluggedIn = true
	opts.WakeToCharge = true
	c.SetChargingOptions(opts)

	ctx := context.Background()

	// Sun recorded by earlier ticks.
	for _, at := range []time.Time{now.Add(-25 * time.Minute), now.Add(-5 * time.Minute)} {
		if err := st.AddSolarReading(ctx, at, 2160, 9); err != nil {
			t.Fatalf("AddSolarReading: %v", err)
		}
	}

	// The car we just woke: plugged in, mid-charge SoC, last event said offline.
	offline := false
	seen := now.Add(-2 * time.Minute)
	v := domain.NewVehicleState(testVIN, seen)
	v.ChargingState = domain.StateStopped
	v.BatteryLevelPercent = intPtr(63)
	v.Online, v.OnlineAt = &offline, &seen
	if err := st.SaveVehicleState(ctx, v); err != nil {
		t.Fatalf("SaveVehicleState: %v", err)
	}

	// The car answers the wake. No tick happens; the event alone must carry it.
	topic, raw := connectivityFrame(t, true, now)
	if err := c.HandleRecord(ctx, topic, raw); err != nil {
		t.Fatalf("HandleRecord: %v", err)
	}

	if len(cmd.starts) != 1 || cmd.starts[0] != 9 {
		t.Fatalf("starts = %v, want [9] — the online event must start the charge before the car sleeps again", cmd.starts)
	}
}

// A plug-in used to wait out the remainder of the tick. The telemetry frame reporting the car
// charging must trigger a solar match on arrival.
func TestAPlugInEventIsActedOnImmediately(t *testing.T) {
	now := time.Date(2026, 8, 16, 19, 41, 0, 0, time.UTC)

	solar := &fakeSolar{}
	cmd := &fakeCommander{}
	c, st := newController(t, solar, cmd)
	c.now = func() time.Time { return now }

	ctx := context.Background()
	if err := st.AddSolarReading(ctx, now.Add(-5*time.Minute), 2160, 9); err != nil {
		t.Fatalf("AddSolarReading: %v", err)
	}

	// The car arrives and starts itself at its remembered current, as Teslas do.
	frame, err := proto.Marshal(&protos.Payload{
		Vin: testVIN, CreatedAt: timestamppb.New(now),
		Data: []*protos.Datum{
			{Key: protos.Field_DetailedChargeState, Value: &protos.Value{
				Value: &protos.Value_DetailedChargeStateValue{
					DetailedChargeStateValue: protos.DetailedChargeStateValue_DetailedChargeStateCharging}}},
			{Key: protos.Field_BatteryLevel, Value: &protos.Value{Value: &protos.Value_IntValue{IntValue: 63}}},
		},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if err := c.HandleRecord(ctx, "evsolar_V", frame); err != nil {
		t.Fatalf("HandleRecord: %v", err)
	}

	if len(cmd.setAmps) != 1 || cmd.setAmps[0] != 9 {
		t.Fatalf("setAmps = %v, want [9] — the plug-in frame must be matched to solar on arrival", cmd.setAmps)
	}
}

// Frames arrive every minute during charging; the already-at-target guard is what keeps event-
// driven evaluation from becoming command churn.
func TestRepeatedFramesDoNotRepeatCommands(t *testing.T) {
	now := time.Date(2026, 8, 16, 19, 41, 0, 0, time.UTC)

	solar := &fakeSolar{}
	cmd := &fakeCommander{}
	c, st := newController(t, solar, cmd)
	c.now = func() time.Time { return now }

	ctx := context.Background()
	if err := st.AddSolarReading(ctx, now.Add(-5*time.Minute), 2160, 9); err != nil {
		t.Fatalf("AddSolarReading: %v", err)
	}

	frame, _ := proto.Marshal(&protos.Payload{
		Vin: testVIN, CreatedAt: timestamppb.New(now),
		Data: []*protos.Datum{
			{Key: protos.Field_DetailedChargeState, Value: &protos.Value{
				Value: &protos.Value_DetailedChargeStateValue{
					DetailedChargeStateValue: protos.DetailedChargeStateValue_DetailedChargeStateCharging}}},
			{Key: protos.Field_ChargeAmps, Value: &protos.Value{Value: &protos.Value_IntValue{IntValue: 9}}},
		},
	})

	for i := 0; i < 5; i++ {
		if err := c.HandleRecord(ctx, "evsolar_V", frame); err != nil {
			t.Fatalf("HandleRecord: %v", err)
		}
	}

	if len(cmd.setAmps) != 1 {
		t.Fatalf("setAmps = %v, want exactly one command across five identical frames", cmd.setAmps)
	}
}
