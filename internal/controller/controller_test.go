package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/teslamotors/fleet-telemetry/protos"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/bbarber/EvSolarChargeController/internal/domain"
	"github.com/bbarber/EvSolarChargeController/internal/enphase"
	"github.com/bbarber/EvSolarChargeController/internal/store"
)

const testVIN = "5YJ3E1EA3KF428848"

// 14:00 UTC is 09:00 America/Chicago in August — the first tick of the day.
var testNow = time.Date(2026, 8, 11, 14, 0, 0, 0, time.UTC)

type fakeSolar struct {
	result enphase.Result
	calls  int
}

func (f *fakeSolar) CurrentProduction(ctx context.Context, now time.Time) enphase.Result {
	f.calls++
	return f.result
}

func watts(w float64, at time.Time) enphase.Result {
	return enphase.Result{Production: &enphase.Production{Watts: w, ReadingAt: at}}
}

type fakeCommander struct {
	setAmps  []int
	stops    int
	starts   []int
	wakes    []string
	failWith error
}

func (f *fakeCommander) Wake(ctx context.Context, vin string) error {
	if f.failWith != nil {
		return f.failWith
	}
	f.wakes = append(f.wakes, vin)
	return nil
}

func (f *fakeCommander) SetChargingAmps(ctx context.Context, vin string, amps int) error {
	if f.failWith != nil {
		return f.failWith
	}
	f.setAmps = append(f.setAmps, amps)
	return nil
}
func (f *fakeCommander) StopCharging(ctx context.Context, vin string) error {
	if f.failWith != nil {
		return f.failWith
	}
	f.stops++
	return nil
}
func (f *fakeCommander) StartCharging(ctx context.Context, vin string, amps int) error {
	if f.failWith != nil {
		return f.failWith
	}
	f.starts = append(f.starts, amps)
	return nil
}

func newController(t *testing.T, solar SolarReader, cmd Commander) (*Controller, *store.Store) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	window, err := domain.NewPollingWindow(domain.DefaultPollingWindowOptions())
	if err != nil {
		t.Fatalf("NewPollingWindow: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(st, solar, cmd, window, domain.DefaultChargingOptions(), log), st
}

func chargingVehicle(t *testing.T, st *store.Store, at time.Time) {
	t.Helper()
	v := domain.NewVehicleState(testVIN, at)
	v.ChargingState = domain.StateCharging
	v.BatteryLevelPercent = intPtr(50)
	if err := st.SaveVehicleState(context.Background(), v); err != nil {
		t.Fatalf("SaveVehicleState: %v", err)
	}
}

func intPtr(v int) *int { return &v }

// ---------------------------------------------------------------------------
// Schedule
// ---------------------------------------------------------------------------

func TestNextTickLandsOnTheScheduledMinutes(t *testing.T) {
	cases := map[string]int{
		"2026-08-11T14:00:00Z": 20,
		"2026-08-11T14:05:00Z": 20,
		"2026-08-11T14:25:00Z": 40,
		"2026-08-11T14:41:00Z": 0,
		"2026-08-11T14:59:00Z": 0,
	}

	for input, wantMinute := range cases {
		now, err := time.Parse(time.RFC3339, input)
		if err != nil {
			t.Fatalf("parsing %s: %v", input, err)
		}
		got := NextTick(now)
		if got.Minute() != wantMinute {
			t.Errorf("NextTick(%s) = %s, want minute %d", input, got.Format(time.RFC3339), wantMinute)
		}
		if !got.After(now) {
			t.Errorf("NextTick(%s) = %s, which is not in the future", input, got)
		}
	}
}

// This is the guard the C# suite kept as an assertion on the NCRONTAB string: the schedule and the
// window must not drift apart, because together they decide how much of the Enphase monthly budget
// gets spent. Stated here in the terms that actually matter.
func TestTheScheduleFitsTheEnphaseMonthlyBudget(t *testing.T) {
	window := domain.DefaultPollingWindowOptions()
	opts := enphase.DefaultOptions()

	ticksPerDay := len(TickMinutes) * (window.EndHourLocal - window.StartHourLocal)
	worstCaseMonth := ticksPerDay * 31

	if ticksPerDay != 27 {
		t.Errorf("ticks per day = %d, want 27 (3 per hour across a 9-hour window)", ticksPerDay)
	}
	if worstCaseMonth > opts.MonthlyCallBudget {
		t.Errorf("worst-case month is %d calls, over the %d budget", worstCaseMonth, opts.MonthlyCallBudget)
	}
	// The Watt plan's real cap. The budget exists to stay under it even if this drifts.
	if worstCaseMonth > 1000 {
		t.Errorf("worst-case month is %d calls, over the Watt plan's 1000 cap", worstCaseMonth)
	}
}

// ---------------------------------------------------------------------------
// Evaluate
// ---------------------------------------------------------------------------

func TestOutsideTheWindowNoEnphaseCallIsSpent(t *testing.T) {
	solar := &fakeSolar{result: watts(3840, testNow)}
	c, st := newController(t, solar, &fakeCommander{})
	chargingVehicle(t, st, testNow)

	// 03:00 UTC is 22:00 the previous day in Chicago.
	if err := c.Evaluate(context.Background(), time.Date(2026, 8, 11, 3, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if solar.calls != 0 {
		t.Errorf("enphase calls = %d, want 0 outside the window", solar.calls)
	}
}

// The window bounds the Enphase poll, not the controller's authority. Overnight the trailing
// readings have aged out, so there is no solar data — but the state-of-charge cap must still hold,
// or a car plugged in before dawn charges to 100% on grid power before the loop first runs.
func TestTheSocCapIsEnforcedOutsideTheWindow(t *testing.T) {
	solar := &fakeSolar{result: watts(3840, testNow)}
	cmd := &fakeCommander{}
	c, st := newController(t, solar, cmd)

	night := time.Date(2026, 8, 11, 8, 0, 0, 0, time.UTC) // 03:00 America/Chicago

	v := domain.NewVehicleState(testVIN, night)
	v.ChargingState = domain.StateCharging
	v.BatteryLevelPercent = intPtr(85) // above the 80% cap
	if err := st.SaveVehicleState(context.Background(), v); err != nil {
		t.Fatalf("SaveVehicleState: %v", err)
	}

	if err := c.Evaluate(context.Background(), night); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if solar.calls != 0 {
		t.Errorf("enphase calls = %d, want 0 — the poll stays inside the window", solar.calls)
	}
	if cmd.stops != 1 {
		t.Fatalf("stops = %d, want 1 — the cap must be enforced at any hour", cmd.stops)
	}
	got, _ := st.GetVehicleState(context.Background(), testVIN)
	if got.Session != domain.SessionStoppedAtCap {
		t.Errorf("Session = %v, want StoppedAtCap", got.Session)
	}
}

// This car charges on solar or not at all. Once the sun is down there is nothing to match, so a
// running session is stopped even well below the state-of-charge cap — that is the whole point.
func TestOvernightChargingIsStoppedEvenBelowTheCap(t *testing.T) {
	cmd := &fakeCommander{}
	c, st := newController(t, &fakeSolar{}, cmd)

	night := time.Date(2026, 8, 11, 8, 0, 0, 0, time.UTC) // 03:00 America/Chicago
	chargingVehicle(t, st, night)                         // 50% SoC, charging

	if err := c.Evaluate(context.Background(), night); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if cmd.stops != 1 {
		t.Fatalf("stops = %d, want 1 — charging past sunset is grid charging", cmd.stops)
	}

	// Recorded as a sun stop, not a cap stop, so it resumes by itself in the morning.
	got, _ := st.GetVehicleState(context.Background(), testVIN)
	if got.Session != domain.SessionStoppedForSun {
		t.Errorf("Session = %v, want StoppedForSun — StoppedAtCap would block the morning resume", got.Session)
	}
}

// A failed poll *inside* the window is not the same as sunset: missing data is not evidence of
// missing production, so a running session must survive it.
func TestAFailedPollInsideTheWindowDoesNotStopTheSession(t *testing.T) {
	solar := &fakeSolar{result: enphase.Result{Reason: enphase.ReasonTransport, Message: "boom"}}
	cmd := &fakeCommander{}
	c, st := newController(t, solar, cmd)
	chargingVehicle(t, st, testNow)

	if err := c.Evaluate(context.Background(), testNow); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if cmd.stops != 0 {
		t.Errorf("stops = %d, want 0 — a transport error is not sunset", cmd.stops)
	}
}

// The opt-in start has to survive the whole path, not just the decision: a plugged-in idle car
// with sun available should produce an actual StartCharging command.
func TestStartWhenPluggedInIssuesARealCommand(t *testing.T) {
	solar := &fakeSolar{result: watts(1440, testNow)} // 6A
	cmd := &fakeCommander{}
	c, st := newController(t, solar, cmd)

	opts := domain.DefaultChargingOptions()
	opts.StartWhenPluggedIn = true
	c.SetChargingOptions(opts)

	v := domain.NewVehicleState(testVIN, testNow)
	v.ChargingState = domain.StateStopped // plugged in, idle, nobody stopped it but the driver
	v.BatteryLevelPercent = intPtr(63)
	online, at := true, testNow.Add(-5*time.Minute)
	v.Online, v.OnlineAt = &online, &at
	if err := st.SaveVehicleState(context.Background(), v); err != nil {
		t.Fatalf("SaveVehicleState: %v", err)
	}

	if err := c.Evaluate(context.Background(), testNow); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if len(cmd.starts) != 1 || cmd.starts[0] != 6 {
		t.Fatalf("starts = %v, want [6]", cmd.starts)
	}
	got, _ := st.GetVehicleState(context.Background(), testVIN)
	if got.LastSetAmps == nil || *got.LastSetAmps != 6 {
		t.Errorf("LastSetAmps = %v, want 6", got.LastSetAmps)
	}
}

// Same situation with the flag off must stay silent — this is the behaviour observed in the field.
func TestIdleCarIsLeftAloneWhenTheFlagIsOff(t *testing.T) {
	solar := &fakeSolar{result: watts(1440, testNow)}
	cmd := &fakeCommander{}
	c, st := newController(t, solar, cmd)

	v := domain.NewVehicleState(testVIN, testNow)
	v.ChargingState = domain.StateStopped
	v.BatteryLevelPercent = intPtr(63)
	if err := st.SaveVehicleState(context.Background(), v); err != nil {
		t.Fatalf("SaveVehicleState: %v", err)
	}

	if err := c.Evaluate(context.Background(), testNow); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if len(cmd.starts) != 0 || len(cmd.setAmps) != 0 || cmd.stops != 0 {
		t.Errorf("commands were sent with the flag off: %+v", cmd)
	}
}

// The wake path end to end: an asleep, plugged-in car on a permitted day with sustained sun
// should produce a real wake, be recorded against the daily limit, and NOT be commanded — the next
// tick sees an online car and takes it from there.
func TestWakesASleepingPluggedInCar(t *testing.T) {
	friday := time.Date(2026, 8, 14, 18, 0, 0, 0, time.UTC) // 13:00 America/Chicago, a Friday
	solar := &fakeSolar{result: watts(2160, friday)}        // 9A
	cmd := &fakeCommander{}
	c, st := newController(t, solar, cmd)

	opts := domain.DefaultChargingOptions()
	opts.WakeToCharge = true
	c.SetChargingOptions(opts)

	// Two readings above the minimum, so the window is sustained rather than a sunbreak.
	ctx := context.Background()
	for _, at := range []time.Time{friday.Add(-15 * time.Minute), friday.Add(-5 * time.Minute)} {
		if err := st.AddSolarReading(ctx, at, 2160, 9); err != nil {
			t.Fatalf("AddSolarReading: %v", err)
		}
	}

	offline := false
	seen := friday.Add(-30 * time.Minute)
	v := domain.NewVehicleState(testVIN, seen)
	v.ChargingState = domain.StateStopped
	v.BatteryLevelPercent = intPtr(55)
	v.Online, v.OnlineAt = &offline, &seen
	if err := st.SaveVehicleState(ctx, v); err != nil {
		t.Fatalf("SaveVehicleState: %v", err)
	}

	if err := c.Evaluate(ctx, friday); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if len(cmd.wakes) != 1 || cmd.wakes[0] != testVIN {
		t.Fatalf("wakes = %v, want one for %s", cmd.wakes, testVIN)
	}
	if len(cmd.starts) != 0 || len(cmd.setAmps) != 0 {
		t.Errorf("a sleeping car was commanded as well as woken: %+v", cmd)
	}

	midnight := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	if n, _ := st.WakesSince(ctx, testVIN, midnight); n != 1 {
		t.Errorf("wakes recorded = %d, want 1", n)
	}
	got, _ := st.GetVehicleState(ctx, testVIN)
	if got.LastWakeAt == nil {
		t.Error("LastWakeAt not recorded; the cooldown would never apply")
	}
}

// Same situation on a Wednesday must not wake.
func TestDoesNotWakeOnADisallowedDay(t *testing.T) {
	wednesday := time.Date(2026, 8, 12, 18, 0, 0, 0, time.UTC)
	solar := &fakeSolar{result: watts(2160, wednesday)}
	cmd := &fakeCommander{}
	c, st := newController(t, solar, cmd)

	opts := domain.DefaultChargingOptions()
	opts.WakeToCharge = true
	c.SetChargingOptions(opts)

	ctx := context.Background()
	for _, at := range []time.Time{wednesday.Add(-15 * time.Minute), wednesday.Add(-5 * time.Minute)} {
		if err := st.AddSolarReading(ctx, at, 2160, 9); err != nil {
			t.Fatalf("AddSolarReading: %v", err)
		}
	}

	offline := false
	seen := wednesday.Add(-30 * time.Minute)
	v := domain.NewVehicleState(testVIN, seen)
	v.ChargingState = domain.StateStopped
	v.BatteryLevelPercent = intPtr(55)
	v.Online, v.OnlineAt = &offline, &seen
	if err := st.SaveVehicleState(ctx, v); err != nil {
		t.Fatalf("SaveVehicleState: %v", err)
	}

	if err := c.Evaluate(ctx, wednesday); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(cmd.wakes) != 0 {
		t.Errorf("woke on a Wednesday: %v", cmd.wakes)
	}
}

func TestASuccessfulCycleRecordsAndCommands(t *testing.T) {
	solar := &fakeSolar{result: watts(2880, testNow)} // 2880W / 240V = 12A
	cmd := &fakeCommander{}
	c, st := newController(t, solar, cmd)
	chargingVehicle(t, st, testNow)

	if err := c.Evaluate(context.Background(), testNow); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if len(cmd.setAmps) != 1 || cmd.setAmps[0] != 12 {
		t.Fatalf("setAmps = %v, want [12]", cmd.setAmps)
	}

	got, err := st.GetVehicleState(context.Background(), testVIN)
	if err != nil {
		t.Fatalf("GetVehicleState: %v", err)
	}
	if got.LastSetAmps == nil || *got.LastSetAmps != 12 {
		t.Errorf("LastSetAmps = %v, want 12", got.LastSetAmps)
	}
	if got.LastSetAt == nil {
		t.Error("LastSetAt was not recorded — override detection needs it to date the settle window")
	}
}

func TestAFailedPollDoesNotStopARunningSession(t *testing.T) {
	solar := &fakeSolar{result: enphase.Result{Reason: enphase.ReasonTransport, Message: "boom"}}
	cmd := &fakeCommander{}
	c, st := newController(t, solar, cmd)
	chargingVehicle(t, st, testNow)

	// A reading from earlier in the window is still valid.
	if err := st.AddSolarReading(context.Background(), testNow.Add(-20*time.Minute), 2640, 11); err != nil {
		t.Fatalf("AddSolarReading: %v", err)
	}

	if err := c.Evaluate(context.Background(), testNow); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if cmd.stops != 0 {
		t.Errorf("stops = %d, want 0 — a failed poll is not evidence of low production", cmd.stops)
	}
	if len(cmd.setAmps) != 1 || cmd.setAmps[0] != 11 {
		t.Errorf("setAmps = %v, want [11] from the surviving reading", cmd.setAmps)
	}
}

func TestLowSolarStopsAndRecordsItsOwnMarker(t *testing.T) {
	solar := &fakeSolar{result: watts(240, testNow)} // 1A, under the 5A minimum
	cmd := &fakeCommander{}
	c, st := newController(t, solar, cmd)
	chargingVehicle(t, st, testNow)

	if err := c.Evaluate(context.Background(), testNow); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if cmd.stops != 1 {
		t.Fatalf("stops = %d, want 1", cmd.stops)
	}

	got, _ := st.GetVehicleState(context.Background(), testVIN)
	if got.Session != domain.SessionStoppedForSun {
		t.Errorf("Session = %v, want StoppedForSun — the resume path depends on it", got.Session)
	}
}

func TestSocCapStopsAndRecordsTheSocMarker(t *testing.T) {
	solar := &fakeSolar{result: watts(3840, testNow)}
	cmd := &fakeCommander{}
	c, st := newController(t, solar, cmd)

	v := domain.NewVehicleState(testVIN, testNow)
	v.ChargingState = domain.StateCharging
	v.BatteryLevelPercent = intPtr(85)
	if err := st.SaveVehicleState(context.Background(), v); err != nil {
		t.Fatalf("SaveVehicleState: %v", err)
	}

	if err := c.Evaluate(context.Background(), testNow); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if cmd.stops != 1 {
		t.Fatalf("stops = %d, want 1", cmd.stops)
	}
	got, _ := st.GetVehicleState(context.Background(), testVIN)
	if got.Session != domain.SessionStoppedAtCap {
		t.Errorf("Session = %v, want StoppedAtCap — StoppedForSun would auto-resume past the cap", got.Session)
	}
}

func TestResumeClearsTheLowSolarMarker(t *testing.T) {
	// The marker must be cleared as part of resuming, or the next telemetry frame showing
	// Charging would be read as a person restarting the session by hand.
	solar := &fakeSolar{result: watts(2640, testNow)} // 11A
	cmd := &fakeCommander{}
	c, st := newController(t, solar, cmd)

	stopped := testNow.Add(-time.Hour)
	v := domain.NewVehicleState(testVIN, testNow)
	v.ChargingState = domain.StateStopped
	v.BatteryLevelPercent = intPtr(50)
	v.Session = domain.SessionStoppedForSun
	v.SessionSince = &stopped
	if err := st.SaveVehicleState(context.Background(), v); err != nil {
		t.Fatalf("SaveVehicleState: %v", err)
	}

	if err := c.Evaluate(context.Background(), testNow); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if len(cmd.starts) != 1 || cmd.starts[0] != 11 {
		t.Fatalf("starts = %v, want [11]", cmd.starts)
	}
	got, _ := st.GetVehicleState(context.Background(), testVIN)
	if got.Session != domain.SessionAuto {
		t.Errorf("Session = %v, want Auto — StoppedByUs with a charging car reads as a manual restart", got.Session)
	}
}

func TestAFailedCommandLeavesStateUnchanged(t *testing.T) {
	// Recording LastSetAmps for a command the car never got would make the next frame look like
	// a manual override.
	solar := &fakeSolar{result: watts(2880, testNow)}
	cmd := &fakeCommander{failWith: errors.New("vehicle unreachable")}
	c, st := newController(t, solar, cmd)
	chargingVehicle(t, st, testNow)

	if err := c.Evaluate(context.Background(), testNow); err == nil {
		t.Fatal("expected the command failure to surface")
	}

	got, _ := st.GetVehicleState(context.Background(), testVIN)
	if got.LastSetAmps != nil {
		t.Errorf("LastSetAmps = %v, want nil after a failed command", got.LastSetAmps)
	}
}

// ---------------------------------------------------------------------------
// Telemetry ingest
// ---------------------------------------------------------------------------

func telemetryFrame(t *testing.T, at time.Time, data ...*protos.Datum) []byte {
	t.Helper()
	raw, err := proto.Marshal(&protos.Payload{Vin: testVIN, CreatedAt: timestamppb.New(at), Data: data})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return raw
}

func TestTelemetryCreatesAndUpdatesVehicleState(t *testing.T) {
	c, st := newController(t, &fakeSolar{}, &fakeCommander{})

	frame := telemetryFrame(t, testNow,
		&protos.Datum{Key: protos.Field_ChargeAmps, Value: &protos.Value{Value: &protos.Value_IntValue{IntValue: 12}}},
		&protos.Datum{Key: protos.Field_DetailedChargeState, Value: &protos.Value{
			Value: &protos.Value_DetailedChargeStateValue{
				DetailedChargeStateValue: protos.DetailedChargeStateValue_DetailedChargeStateCharging}}},
	)

	if err := c.HandleTelemetry(context.Background(), frame); err != nil {
		t.Fatalf("HandleTelemetry: %v", err)
	}

	got, err := st.GetVehicleState(context.Background(), testVIN)
	if err != nil {
		t.Fatalf("GetVehicleState: %v", err)
	}
	if got == nil {
		t.Fatal("expected a vehicle record to be created")
	}
	if got.ChargingState != domain.StateCharging || got.ChargeAmps == nil || *got.ChargeAmps != 12 {
		t.Errorf("state = %+v, want Charging at 12A", got)
	}
}

func TestTelemetryFlagsAManualOverride(t *testing.T) {
	c, st := newController(t, &fakeSolar{}, &fakeCommander{})

	setAt := testNow.Add(-30 * time.Minute)
	v := domain.NewVehicleState(testVIN, setAt)
	v.ChargingState = domain.StateCharging
	v.LastSetAmps = intPtr(12)
	v.LastSetAt = &setAt
	if err := st.SaveVehicleState(context.Background(), v); err != nil {
		t.Fatalf("SaveVehicleState: %v", err)
	}

	frame := telemetryFrame(t, testNow,
		&protos.Datum{Key: protos.Field_ChargeAmps, Value: &protos.Value{Value: &protos.Value_IntValue{IntValue: 32}}},
		&protos.Datum{Key: protos.Field_DetailedChargeState, Value: &protos.Value{
			Value: &protos.Value_DetailedChargeStateValue{
				DetailedChargeStateValue: protos.DetailedChargeStateValue_DetailedChargeStateCharging}}},
	)

	if err := c.HandleTelemetry(context.Background(), frame); err != nil {
		t.Fatalf("HandleTelemetry: %v", err)
	}

	got, _ := st.GetVehicleState(context.Background(), testVIN)
	if got.Session != domain.SessionOverridden {
		t.Errorf("Session = %v, want Overridden", got.Session)
	}
}

func TestTelemetryRejectsGarbageWithoutTouchingState(t *testing.T) {
	c, st := newController(t, &fakeSolar{}, &fakeCommander{})

	if err := c.HandleTelemetry(context.Background(), []byte{0xff, 0xff, 0xff}); err == nil {
		t.Error("expected a decode error")
	}

	got, _ := st.LatestVehicleState(context.Background())
	if got != nil {
		t.Errorf("a malformed frame created state: %+v", got)
	}
}

func TestOverriddenVehicleIsLeftAloneByTheLoop(t *testing.T) {
	solar := &fakeSolar{result: watts(3840, testNow)}
	cmd := &fakeCommander{}
	c, st := newController(t, solar, cmd)

	v := domain.NewVehicleState(testVIN, testNow)
	v.ChargingState = domain.StateCharging
	v.BatteryLevelPercent = intPtr(50)
	v.Session = domain.SessionOverridden
	if err := st.SaveVehicleState(context.Background(), v); err != nil {
		t.Fatalf("SaveVehicleState: %v", err)
	}

	if err := c.Evaluate(context.Background(), testNow); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if len(cmd.setAmps) != 0 || cmd.stops != 0 || len(cmd.starts) != 0 {
		t.Errorf("commands were sent despite an active override: %+v", cmd)
	}
}
