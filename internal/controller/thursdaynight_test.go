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

// The owner's actual routine: plug in Thursday night, expect the car charging by Friday mid-
// morning. This walks the whole chain against real components — only the car and the sun are
// simulated — because the chain is exactly what broke three separate ways when first traced:
//
//  1. By Friday 10:00 the car has been silent for 13 hours, and the staleness gate refused to
//     consider it at all — which also made the wake path unreachable, since waking hangs off a
//     decision the stale gate preempts.
//  2. Resume did not check connectivity, so once past staleness it would have commanded a
//     sleeping car every 20 minutes, failing each time — and a resume failure also never wakes.
//  3. After the wake, a car whose charge state has not changed sends no telemetry — only a
//     connectivity event — so freshness must count both channels or the woken car is still
//     "stale" at the only moment that matters.
func TestThursdayNightPlugInChargesFridayMorning(t *testing.T) {
	loc, _ := time.LoadLocation("America/Chicago")
	// 2026-08-20 is a Thursday; the 21st is a Friday, a permitted wake day.
	thuNight := time.Date(2026, 8, 20, 21, 0, 0, 0, loc).UTC()

	solar := &fakeSolar{}
	cmd := &fakeCommander{}
	c, st := newController(t, solar, cmd)

	opts := domain.DefaultChargingOptions()
	opts.WakeToCharge = true
	opts.StartWhenPluggedIn = true
	c.SetChargingOptions(opts)

	ctx := context.Background()
	now := thuNight
	c.now = func() time.Time { return now }

	vFrame := func(state protos.DetailedChargeStateValue, soc int32, at time.Time) []byte {
		raw, err := proto.Marshal(&protos.Payload{
			Vin: testVIN, CreatedAt: timestamppb.New(at),
			Data: []*protos.Datum{
				{Key: protos.Field_DetailedChargeState, Value: &protos.Value{
					Value: &protos.Value_DetailedChargeStateValue{DetailedChargeStateValue: state}}},
				{Key: protos.Field_BatteryLevel, Value: &protos.Value{Value: &protos.Value_IntValue{IntValue: soc}}},
			},
		})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		return raw
	}
	connFrame := func(online bool, at time.Time) []byte {
		status := protos.ConnectivityEvent_DISCONNECTED
		if online {
			status = protos.ConnectivityEvent_CONNECTED
		}
		raw, err := proto.Marshal(&protos.VehicleConnectivity{
			Vin: testVIN, Status: status, CreatedAt: timestamppb.New(at)})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		return raw
	}

	// --- Thursday 21:00: plug in. The car starts itself; the event-driven stop ends it. ---
	if err := c.HandleRecord(ctx, "evsolar_V",
		vFrame(protos.DetailedChargeStateValue_DetailedChargeStateCharging, 55, now)); err != nil {
		t.Fatalf("plug-in frame: %v", err)
	}
	if cmd.stops != 1 {
		t.Fatalf("stops = %d, want 1 — charging at night is grid charging", cmd.stops)
	}

	// The car acknowledges the stop, then sleeps around 23:00.
	now = thuNight.Add(10 * time.Minute)
	if err := c.HandleRecord(ctx, "evsolar_V",
		vFrame(protos.DetailedChargeStateValue_DetailedChargeStateStopped, 55, now)); err != nil {
		t.Fatalf("stopped frame: %v", err)
	}
	now = thuNight.Add(2 * time.Hour)
	if err := c.HandleRecord(ctx, "evsolar_connectivity", connFrame(false, now)); err != nil {
		t.Fatalf("sleep event: %v", err)
	}

	got, _ := st.GetVehicleState(ctx, testVIN)
	if got.Session != domain.SessionStoppedForSun {
		t.Fatalf("Session = %v, want StoppedForSun overnight", got.Session)
	}

	// --- Friday morning: ticks poll rising sun. The car stays silent throughout. ---
	friday := func(h, m int) time.Time { return time.Date(2026, 8, 21, h, m, 0, 0, loc).UTC() }
	morning := []struct {
		at    time.Time
		watts float64
	}{
		{friday(9, 40), 720},   // 3.0A — under the floor
		{friday(10, 0), 960},   // 4.0A — still under
		{friday(10, 20), 1320}, // 5.5A — first reading over
		{friday(10, 40), 1440}, // 6.0A — second reading over: sustained
	}
	for _, tick := range morning {
		now = tick.at
		solar.result = watts(tick.watts, tick.at.Add(-2*time.Minute))
		if err := c.Evaluate(ctx, tick.at); err != nil {
			t.Fatalf("tick %s: %v", tick.at.Format("15:04"), err)
		}
	}

	// 13+ hours of silence must not have hidden the car from the wake gates...
	if len(cmd.wakes) != 1 {
		t.Fatalf("wakes = %v, want exactly one by 10:40 — the stale gate must not bury an asleep car", cmd.wakes)
	}
	// ...and no command may have been thrown at it while it slept.
	if len(cmd.starts) != 0 && cmd.stops == 1 {
		t.Fatalf("starts = %v before the car was awake", cmd.starts)
	}

	// --- The car answers the wake. Its charge state is unchanged, so ONLY connectivity speaks. ---
	now = friday(10, 41)
	if err := c.HandleRecord(ctx, "evsolar_connectivity", connFrame(true, now)); err != nil {
		t.Fatalf("wake answer: %v", err)
	}

	if len(cmd.starts) != 1 || cmd.starts[0] != 6 {
		t.Fatalf("starts = %v, want [6] — the online event must resume the session while the car is awake", cmd.starts)
	}
	got, _ = st.GetVehicleState(ctx, testVIN)
	if got.Session != domain.SessionAuto {
		t.Errorf("Session = %v, want Auto after our own resume", got.Session)
	}
	if got.LastSetAmps == nil || *got.LastSetAmps != 6 {
		t.Errorf("LastSetAmps = %v, want 6", got.LastSetAmps)
	}
}
