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

type fakeRecorder struct {
	charges     []int
	chargeWatts []float64
}

func (f *fakeRecorder) RecordStatus(ctx context.Context, v *domain.VehicleState) {}
func (f *fakeRecorder) RecordSolar(ctx context.Context, at time.Time, watts, amps float64, houseWatts *float64) {
}
func (f *fakeRecorder) RecordEvent(ctx context.Context, at time.Time, vin, k, a, r string) {}
func (f *fakeRecorder) RecordCharge(ctx context.Context, at time.Time, vin string, amps int, watts float64) {
	f.charges = append(f.charges, amps)
	f.chargeWatts = append(f.chargeWatts, watts)
}

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
		if err := st.AddSolarReading(ctx, at, 2160, 9, nil); err != nil {
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
	if err := st.AddSolarReading(ctx, now.Add(-5*time.Minute), 2160, 9, nil); err != nil {
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
	if err := st.AddSolarReading(ctx, now.Add(-5*time.Minute), 2160, 9, nil); err != nil {
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

// Two cars, one wall connector. The car being driven around town reports constantly — battery
// level changes while driving — and must not out-shout the one sitting plugged in at home.
// Before per-VIN evaluation, the loop acted on whichever car reported most recently.
func TestTheDrivenCarDoesNotEclipseTheChargingOne(t *testing.T) {
	const bessie = "7SAYGDEEXPA069171"
	now := time.Date(2026, 8, 16, 19, 41, 0, 0, time.UTC)

	solar := &fakeSolar{}
	cmd := &fakeCommander{}

	st, err := storeOpen(t)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	window, _ := domain.NewPollingWindow(domain.DefaultPollingWindowOptions())
	log := quietLog()
	c := New([]string{testVIN, bessie}, st, solar, cmd, window, domain.DefaultChargingOptions(), log)
	c.now = func() time.Time { return now }

	ctx := context.Background()
	if err := st.AddSolarReading(ctx, now.Add(-5*time.Minute), 2160, 9, nil); err != nil {
		t.Fatalf("AddSolarReading: %v", err)
	}

	// Tessie: plugged in and charging at home, reported ten minutes ago.
	tessie := domain.NewVehicleState(testVIN, now.Add(-10*time.Minute))
	tessie.ChargingState = domain.StateCharging
	tessie.BatteryLevelPercent = intPtr(50)
	if err := st.SaveVehicleState(ctx, tessie); err != nil {
		t.Fatalf("SaveVehicleState: %v", err)
	}

	// Bessie: being driven, reporting RIGHT NOW — the more recent reporter.
	driving := domain.NewVehicleState(bessie, now)
	driving.ChargingState = domain.StateDisconnected
	driving.BatteryLevelPercent = intPtr(70)
	if err := st.SaveVehicleState(ctx, driving); err != nil {
		t.Fatalf("SaveVehicleState: %v", err)
	}

	if err := c.Evaluate(ctx, now); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if len(cmd.setAmps) != 1 || cmd.setAmps[0] != 9 {
		t.Fatalf("setAmps = %v, want [9] — Tessie's session must be managed despite Bessie reporting later", cmd.setAmps)
	}
	if cmd.stops != 0 && len(cmd.starts) == 0 {
		t.Errorf("unexpected commands: %+v", cmd)
	}
}

// Every telemetry fold produces one draw sample, and the zero on a stop is what closes the step
// on the dashboard graph — without it the chart holds the last current forever.
func TestTelemetryFoldsEmitChargeSamples(t *testing.T) {
	now := time.Date(2026, 8, 18, 19, 41, 0, 0, time.UTC)
	c, st := newController(t, &fakeSolar{}, &fakeCommander{})
	c.now = func() time.Time { return now }
	rec := &fakeRecorder{}
	c.SetRecorder(rec)
	_ = st

	ctx := context.Background()
	frame := func(state protos.DetailedChargeStateValue, amps int32) []byte {
		raw, _ := proto.Marshal(&protos.Payload{
			Vin: testVIN, CreatedAt: timestamppb.New(now),
			Data: []*protos.Datum{
				{Key: protos.Field_DetailedChargeState, Value: &protos.Value{
					Value: &protos.Value_DetailedChargeStateValue{DetailedChargeStateValue: state}}},
				{Key: protos.Field_ChargeAmps, Value: &protos.Value{Value: &protos.Value_IntValue{IntValue: amps}}},
			},
		})
		return raw
	}

	if err := c.HandleRecord(ctx, "evsolar_V", frame(protos.DetailedChargeStateValue_DetailedChargeStateCharging, 12)); err != nil {
		t.Fatalf("charging frame: %v", err)
	}
	if err := c.HandleRecord(ctx, "evsolar_V", frame(protos.DetailedChargeStateValue_DetailedChargeStateStopped, 12)); err != nil {
		t.Fatalf("stopped frame: %v", err)
	}

	if len(rec.charges) != 2 || rec.charges[0] != 12 || rec.charges[1] != 0 {
		t.Fatalf("charge samples = %v, want [12 0] — the zero closes the step", rec.charges)
	}
	if rec.chargeWatts[0] != 12*240 {
		t.Errorf("watts = %v, want amps x the configured voltage", rec.chargeWatts[0])
	}
}
