package domain

import (
	"testing"
	"time"
)

// The home gate. Without it, this controller stops any charging session anywhere the moment the
// sun sets — including a Supercharger stop on a road trip, which the owner then has to restart
// from the app at every charging stop for the rest of the drive.

func homeOptions() ChargingOptions {
	o := DefaultChargingOptions()
	// A house in Lincoln, NE. The gate compares in-process; only the boolean is ever stored.
	o.HomeLatitude = 40.75
	o.HomeLongitude = -96.65
	return o
}

func atHomeVehicle(opts ...vehOpt) *VehicleState {
	home := true
	v := decisionVehicle(opts...)
	v.AtHome = &home
	return v
}

func TestHomeSessionsAreManaged(t *testing.T) {
	d := Decide(atHomeVehicle(), amps(12), true, homeOptions(), testNow)

	if d.Action != ActionSetAmps {
		t.Errorf("Action = %v, want SetAmps — the home session must still be managed", d.Action)
	}
}

// A Supercharger stop at night: charging, sun down, far from home. The sunset stop must not fire.
func TestARoadTripSuperchargerSessionIsNeverStopped(t *testing.T) {
	away := false
	fast := true
	v := decisionVehicle() // Charging
	v.AtHome = &away
	v.FastCharger = &fast

	d := Decide(v, nil, false, homeOptions(), testNow) // night, no solar data

	if d.Action != ActionSkipNotAtHome {
		t.Fatalf("Action = %v, want SkipNotAtHome — a road-trip session is not ours to stop", d.Action)
	}
	if d.ShouldStop() || d.ShouldSend() {
		t.Error("commanded a session away from home")
	}
}

// A friend's L2 at night: AC, so FastCharger is false — the position alone must protect it.
func TestAFriendsChargerIsNeverStopped(t *testing.T) {
	away := false
	v := decisionVehicle()
	v.AtHome = &away

	d := Decide(v, nil, false, homeOptions(), testNow)

	if d.Action != ActionSkipNotAtHome {
		t.Errorf("Action = %v, want SkipNotAtHome", d.Action)
	}
}

// A DC session is untouchable even if the coordinates say home (a mobile DC unit, GPS noise):
// there is no world where cutting a fast charge to 16A is what anyone wants.
func TestADCSessionIsUntouchableEvenAtHome(t *testing.T) {
	fast := true
	v := atHomeVehicle()
	v.FastCharger = &fast

	d := Decide(v, amps(12), true, homeOptions(), testNow)

	if d.Action != ActionSkipNotAtHome {
		t.Errorf("Action = %v, want SkipNotAtHome for a DC session", d.Action)
	}
}

// Until a position has been reported, commanding is withheld — the gate fails safe, and the
// position arrives within a poll interval of the car being awake.
func TestAnUnknownPositionWithholdsCommands(t *testing.T) {
	v := decisionVehicle() // AtHome nil

	d := Decide(v, amps(12), true, homeOptions(), testNow)

	if d.Action != ActionSkipNotAtHome {
		t.Errorf("Action = %v, want SkipNotAtHome while the position is unknown", d.Action)
	}
}

// With no home configured the gate is off entirely — the behaviour every existing test pins.
func TestNoHomeConfiguredMeansNoGate(t *testing.T) {
	d := Decide(decisionVehicle(), amps(12), true, decisionOptions(), testNow)

	if d.Action != ActionSetAmps {
		t.Errorf("Action = %v, want SetAmps with the gate unconfigured", d.Action)
	}
}

// The fold computes AtHome from a position frame and stores only the boolean.
func TestTheFoldComputesAtHome(t *testing.T) {
	opts := homeOptions()

	cases := []struct {
		name     string
		lat, lon float64
		want     bool
	}{
		{"in the driveway", 40.7501, -96.6501, true},
		{"a block away", 40.7515, -96.6520, false},
		{"across the country", 36.16, -115.15, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newState()
			got := ApplyObservation(s, Observation{
				VIN: testVIN, ObservedAt: testNow,
				Latitude: &c.lat, Longitude: &c.lon,
			}, opts)

			if got.AtHome == nil || *got.AtHome != c.want {
				t.Errorf("AtHome = %v, want %v", got.AtHome, c.want)
			}
		})
	}
}

// A frame without a position leaves the last known answer intact, and no position is ever
// computed when the gate is unconfigured.
func TestAtHomeIsSticky(t *testing.T) {
	opts := homeOptions()
	s := newState()
	lat, lon := 40.7501, -96.6501
	ApplyObservation(s, Observation{VIN: testVIN, ObservedAt: testNow, Latitude: &lat, Longitude: &lon}, opts)

	got := ApplyObservation(s, Observation{VIN: testVIN, ObservedAt: testNow.Add(time.Minute)}, opts)
	if got.AtHome == nil || !*got.AtHome {
		t.Error("a frame without a position wiped the last known answer")
	}

	unconfigured := newState()
	ApplyObservation(unconfigured, Observation{VIN: testVIN, ObservedAt: testNow, Latitude: &lat, Longitude: &lon},
		DefaultChargingOptions())
	if unconfigured.AtHome != nil {
		t.Error("AtHome was computed with no home configured")
	}
}

// The wake gate must refuse a car last seen away — a wake there is at best wasted money.
func TestNeverWakesACarAwayFromHome(t *testing.T) {
	opts := homeOptions()
	opts.WakeToCharge = true

	v := wakeReady()
	away := false
	v.AtHome = &away

	if d := DecideWake(v, goodInputs(wakeFriday), opts); d.Wake {
		t.Error("woke a car last seen away from home")
	}

	home := true
	v.AtHome = &home
	if d := DecideWake(v, goodInputs(wakeFriday), opts); !d.Wake {
		t.Errorf("refused to wake the car at home: %s", d.Reason)
	}
}
