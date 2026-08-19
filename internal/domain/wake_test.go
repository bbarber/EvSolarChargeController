package domain

import (
	"testing"
	"time"
)

// Waking costs $0.02, drains the battery it is meant to fill, and is the one action here that
// reaches out to a car that did not ask to be disturbed. Every gate below exists for a reason, and
// each is tested separately so a future change cannot quietly drop one.

// A Friday afternoon: permitted day, car asleep and plugged in at 55%, sun sustained.
func wakeReady(opts ...vehOpt) *VehicleState {
	offline := false
	seen := wakeFriday.Add(-30 * time.Minute)
	v := &VehicleState{
		VIN:                 testVIN,
		ChargingState:       StateStopped,
		BatteryLevelPercent: intPtr(55),
		Online:              &offline,
		OnlineAt:            &seen,
		LastUpdated:         seen,
	}
	for _, o := range opts {
		o(v)
	}
	return v
}

// 2026-08-14 is a Friday.
var wakeFriday = time.Date(2026, 8, 14, 13, 0, 0, 0, time.UTC)

func wakeOptions() ChargingOptions {
	o := DefaultChargingOptions()
	o.WakeToCharge = true
	return o
}

func goodInputs(now time.Time) WakeInputs {
	a := 9.0
	return WakeInputs{MaxAmpsLastWindow: &a, ReadingsAboveMinimum: 2, WakesToday: 0, LocalNow: now}
}

func TestWakesWhenEveryGatePasses(t *testing.T) {
	d := DecideWake(wakeReady(), goodInputs(wakeFriday), wakeOptions())

	if !d.Wake {
		t.Fatalf("expected a wake, got: %s", d.Reason)
	}
}

func TestDoesNotWakeWhenDisabled(t *testing.T) {
	opts := wakeOptions()
	opts.WakeToCharge = false

	if d := DecideWake(wakeReady(), goodInputs(wakeFriday), opts); d.Wake {
		t.Error("woke with the feature disabled")
	}
}

// The day restriction is a human preference about when it is acceptable to be disturbed.
func TestOnlyWakesOnPermittedDays(t *testing.T) {
	// 2026-08-10 Mon … 2026-08-16 Sun
	days := map[string]struct {
		date time.Time
		want bool
	}{
		"Monday":    {time.Date(2026, 8, 10, 13, 0, 0, 0, time.UTC), false},
		"Tuesday":   {time.Date(2026, 8, 11, 13, 0, 0, 0, time.UTC), false},
		"Wednesday": {time.Date(2026, 8, 12, 13, 0, 0, 0, time.UTC), false},
		"Thursday":  {time.Date(2026, 8, 13, 13, 0, 0, 0, time.UTC), false},
		"Friday":    {time.Date(2026, 8, 14, 13, 0, 0, 0, time.UTC), true},
		"Saturday":  {time.Date(2026, 8, 15, 13, 0, 0, 0, time.UTC), true},
		"Sunday":    {time.Date(2026, 8, 16, 13, 0, 0, 0, time.UTC), true},
	}

	for name, c := range days {
		t.Run(name, func(t *testing.T) {
			if c.date.Weekday().String() != name {
				t.Fatalf("test data wrong: %s is a %s", c.date.Format("2006-01-02"), c.date.Weekday())
			}
			d := DecideWake(wakeReady(), goodInputs(c.date), wakeOptions())
			if d.Wake != c.want {
				t.Errorf("Wake = %v, want %v (%s)", d.Wake, c.want, d.Reason)
			}
		})
	}
}

// The two cars have different owners; one accepting a Friday wake must not opt the other in.
func TestPerVinDayOverrideBeatsTheFleetList(t *testing.T) {
	opts := wakeOptions() // fleet-wide Fri/Sat/Sun
	opts.WakeDaysByVIN = map[string][]time.Weekday{
		testVIN: {time.Saturday, time.Sunday},
	}

	if d := DecideWake(wakeReady(), goodInputs(wakeFriday), opts); d.Wake {
		t.Errorf("expected no wake on Friday for a Sat/Sun-only VIN, got: %s", d.Reason)
	}

	saturday := wakeFriday.AddDate(0, 0, 1)
	if d := DecideWake(wakeReady(), goodInputs(saturday), opts); !d.Wake {
		t.Errorf("expected a Saturday wake for a Sat/Sun-only VIN: %s", d.Reason)
	}

	// A VIN not in the map keeps the fleet-wide list.
	other := wakeReady()
	other.VIN = "OTHERVIN123456789"
	if d := DecideWake(other, goodInputs(wakeFriday), opts); !d.Wake {
		t.Errorf("expected the fleet-wide Friday wake for an unlisted VIN: %s", d.Reason)
	}
}

func TestAnEmptyDayListMeansEveryDay(t *testing.T) {
	opts := wakeOptions()
	opts.WakeDays = nil

	monday := time.Date(2026, 8, 10, 13, 0, 0, 0, time.UTC)
	if d := DecideWake(wakeReady(), goodInputs(monday), opts); !d.Wake {
		t.Errorf("expected a wake with no day restriction: %s", d.Reason)
	}
}

func TestRespectsTheDailyLimit(t *testing.T) {
	in := goodInputs(wakeFriday)
	in.WakesToday = 2 // the default limit

	if d := DecideWake(wakeReady(), in, wakeOptions()); d.Wake {
		t.Error("woke past the daily limit")
	}
}

// The failure mode this guards: wake, car comes up, solar dips before the command lands, car
// sleeps, wake again — burning battery and money in a loop.
func TestRespectsTheCooldown(t *testing.T) {
	v := wakeReady()
	recent := wakeFriday.Add(-20 * time.Minute)
	v.LastWakeAt = &recent

	if d := DecideWake(v, goodInputs(wakeFriday), wakeOptions()); d.Wake {
		t.Error("woke inside the cooldown")
	}

	older := wakeFriday.Add(-90 * time.Minute)
	v.LastWakeAt = &older
	if d := DecideWake(v, goodInputs(wakeFriday), wakeOptions()); !d.Wake {
		t.Errorf("expected a wake once the cooldown passed: %s", d.Reason)
	}
}

func TestDoesNotWakeACarThatIsAlreadyOnline(t *testing.T) {
	v := wakeReady()
	online := true
	v.Online = &online

	if d := DecideWake(v, goodInputs(wakeFriday), wakeOptions()); d.Wake {
		t.Error("woke a car that was already online")
	}
}

func TestDoesNotWakeWhenConnectivityWasNeverObserved(t *testing.T) {
	v := wakeReady()
	v.Online = nil

	if d := DecideWake(v, goodInputs(wakeFriday), wakeOptions()); d.Wake {
		t.Error("woke without ever having seen a connectivity event")
	}
}

func TestDoesNotWakeAnUnpluggedCar(t *testing.T) {
	// The wake is wasted, and the car may be parked miles away.
	v := wakeReady(vehState(StateDisconnected))

	if d := DecideWake(v, goodInputs(wakeFriday), wakeOptions()); d.Wake {
		t.Error("woke a car last seen unplugged")
	}
}

func TestDoesNotWakeUnderAnOverride(t *testing.T) {
	if d := DecideWake(wakeReady(vehOverride()), goodInputs(wakeFriday), wakeOptions()); d.Wake {
		t.Error("woke while a manual override was active")
	}
}

// Waking at 79% against an 80% cap spends real money for a few minutes of charging.
func TestDoesNotWakeNearTheSocCap(t *testing.T) {
	v := wakeReady()
	v.BatteryLevelPercent = intPtr(72) // cap 80, headroom 10 → ceiling 70

	if d := DecideWake(v, goodInputs(wakeFriday), wakeOptions()); d.Wake {
		t.Errorf("woke at 72%% with a 70%% ceiling: %s", d.Reason)
	}

	v.BatteryLevelPercent = intPtr(65)
	if d := DecideWake(v, goodInputs(wakeFriday), wakeOptions()); !d.Wake {
		t.Errorf("expected a wake at 65%%: %s", d.Reason)
	}
}

func TestDoesNotWakeWithoutEnoughSun(t *testing.T) {
	in := goodInputs(wakeFriday)
	low := 3.0
	in.MaxAmpsLastWindow = &low

	if d := DecideWake(wakeReady(), in, wakeOptions()); d.Wake {
		t.Error("woke with production below the connector minimum")
	}
}

// One reading above the minimum is a sunbreak, not a window worth waking for.
func TestRequiresASustainedWindowNotASingleReading(t *testing.T) {
	in := goodInputs(wakeFriday)
	in.ReadingsAboveMinimum = 1

	if d := DecideWake(wakeReady(), in, wakeOptions()); d.Wake {
		t.Error("woke on a single reading")
	}
}

func TestDoesNotWakeWithNoSolarData(t *testing.T) {
	in := goodInputs(wakeFriday)
	in.MaxAmpsLastWindow = nil

	if d := DecideWake(wakeReady(), in, wakeOptions()); d.Wake {
		t.Error("woke with no readings at all")
	}
}

func TestDoesNotWakeWithUnknownStateOfCharge(t *testing.T) {
	v := wakeReady()
	v.BatteryLevelPercent = nil

	if d := DecideWake(v, goodInputs(wakeFriday), wakeOptions()); d.Wake {
		t.Error("woke without knowing the state of charge")
	}
}

// Every refusal explains itself, because these will be read in a log at 200 lines a day.
func TestEveryRefusalCarriesAReason(t *testing.T) {
	d := DecideWake(wakeReady(vehOverride()), goodInputs(wakeFriday), wakeOptions())
	if d.Reason == "" {
		t.Error("a refusal with no reason is unreadable in a log")
	}
}
