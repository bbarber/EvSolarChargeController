package domain

import (
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"
)

// What happens when a car arrives at 50% rather than sitting plugged in from midnight.
//
// These scenarios exposed the window being overloaded: it bounded the Enphase poll *and* the
// controller's authority. A car plugged in at 04:30 charged to 100% on grid power before the loop
// first ran, and a car still charging at 18:00 carried on all night.
//
// The loop now ticks around the clock and only the poll is window-bound. With the sun down there
// is nothing to match, so any running session is stopped — this car charges on solar or not at
// all. The stop is recorded as a low-solar stop, so it resumes by itself the next morning.

type plugInScenario struct {
	name string
	// plugInHour is local time the connector goes in.
	plugInHour float64
	// carDefaultAmps is what the vehicle starts drawing on its own. A Tesla begins charging on
	// plug-in at its last-used current unless a schedule says otherwise.
	carDefaultAmps int
}

// plugStep is one 15-minute observation, kept for plotting.
type plugStep struct {
	T          string  `json:"t"`
	H          float64 `json:"h"`
	A          float64 `json:"a"`
	D          int     `json:"d"`
	C          float64 `json:"c"`
	Supervised bool    `json:"sup"`
	Plugged    bool    `json:"plug"`
	K          string  `json:"k"`
}

type plugInOutcome struct {
	Steps            []plugStep
	unsupervisedWh   float64
	solarMatchedWh   float64
	gridWh           float64
	firstCommandAt   string
	commands         int
	finalSoc         float64
	drewBeforeWindow bool
}

func simulatePlugIn(t *testing.T, sc plugInScenario, w weather, opts ChargingOptions, window *PollingWindow) plugInOutcome {
	t.Helper()

	loc, _ := time.LoadLocation("America/Chicago")
	dayStart := time.Date(2026, 8, 11, 0, 0, 0, 0, loc)

	// Starts disconnected: the car is not home yet.
	vehicle := &VehicleState{
		VIN:                 testVIN,
		ChargingState:       StateDisconnected,
		BatteryLevelPercent: intPtr(50),
		ReportedMaxAmps:     intPtr(48),
		LastUpdated:         dayStart,
	}
	socPct := 50.0
	plugged := false

	var readings []struct {
		at   time.Time
		amps float64
	}
	out := plugInOutcome{}

	for i := 0; i < 24*60/simStepMinutes; i++ {
		now := dayStart.Add(time.Duration(i) * stepMinutes())
		hour := float64(now.Hour()) + float64(now.Minute())/60

		if !plugged && hour >= sc.plugInHour {
			// The connector goes in and the car starts on its own. We have set nothing, so this
			// must not read as a manual override — there is no value of ours to contradict.
			charging := StateCharging
			latch := false
			vehicle = ApplyObservation(vehicle, Observation{
				VIN:             testVIN,
				ObservedAt:      now,
				ChargingState:   &charging,
				ReportedAmps:    intPtr(sc.carDefaultAmps),
				LatchDisengaged: &latch,
			}, opts)
			if vehicle.OverrideActive {
				t.Errorf("%s: plugging in was misread as a manual override", sc.name)
			}
			vehicle.LastSetAmps = nil // nothing commanded yet
			plugged = true
		}

		watts := w.peakW * w.factor(hour)
		amps, _ := WattsToAmps(watts, simVoltage)

		draw := 0
		lastAction := "OutsideWindow"
		inWindow := window.IsOpen(now)

		if !plugged {
			lastAction = "NotPluggedIn"
		} else if !inWindow {
			// No poll outside the window, but the loop still decides. With no solar reading,
			// Decide leaves a running session alone — and still enforces the cap.
			d := Decide(vehicle, nil, false, opts, now)
			lastAction = d.Action.String()
			if d.ShouldSend() || d.ShouldResume() || d.ShouldStop() {
				out.commands++
				if out.firstCommandAt == "" {
					out.firstCommandAt = now.Format("15:04")
				}
			}
			result := dayResult{}
			applyDecision(vehicle, d, now, &result)

			if vehicle.ChargingState.IsActivelyCharging() {
				if vehicle.LastSetAmps != nil {
					draw = *vehicle.LastSetAmps
				} else {
					draw = sc.carDefaultAmps
				}
			}
			out.unsupervisedWh += float64(draw) * simVoltage * (simStepMinutes / 60.0)
			if hour < 9 && draw > 0 {
				out.drewBeforeWindow = true
			}
		} else {
			readings = append(readings, struct {
				at   time.Time
				amps float64
			}{now, amps})

			cutoff := now.Add(-opts.LookbackWindow)
			var windowMax *float64
			kept := readings[:0]
			for _, r := range readings {
				if !r.at.Before(cutoff) {
					kept = append(kept, r)
					if windowMax == nil || r.amps > *windowMax {
						v := r.amps
						windowMax = &v
					}
				}
			}
			readings = kept

			d := Decide(vehicle, windowMax, true, opts, now)
			lastAction = d.Action.String()
			if d.ShouldSend() || d.ShouldResume() || d.ShouldStop() {
				out.commands++
				if out.firstCommandAt == "" {
					out.firstCommandAt = now.Format("15:04")
				}
			}
			result := dayResult{}
			applyDecision(vehicle, d, now, &result)

			if vehicle.ChargingState.IsActivelyCharging() {
				if vehicle.LastSetAmps != nil {
					draw = *vehicle.LastSetAmps
				} else {
					draw = sc.carDefaultAmps // still on the car's own figure until we command
				}
			}
			out.solarMatchedWh += float64(draw) * simVoltage * (simStepMinutes / 60.0)
		}

		if shortfall := float64(draw) - amps; shortfall > 0 {
			out.gridWh += shortfall * simVoltage * (simStepMinutes / 60.0)
		}

		socPct = advanceSoc(socPct, draw)
		out.Steps = append(out.Steps, plugStep{
			T: now.Format("15:04"), H: hour, A: amps, D: draw, C: socPct,
			Supervised: inWindow && plugged, Plugged: plugged, K: lastAction,
		})
		vehicle.BatteryLevelPercent = intPtr(int(socPct))
		vehicle.LastUpdated = now
	}

	out.finalSoc = socPct
	return out
}

func stepMinutes() time.Duration { return simStepMinutes * time.Minute }

func TestPlugsInAt50Percent(t *testing.T) {
	opts := DefaultChargingOptions()
	window, err := NewPollingWindow(DefaultPollingWindowOptions())
	if err != nil {
		t.Fatalf("NewPollingWindow: %v", err)
	}

	var clear weather
	for _, w := range weatherProfiles() {
		if w.name == "clear summer day" {
			clear = w
		}
	}

	scenarios := []plugInScenario{
		{name: "04:30 — plugged in before dawn", plugInHour: 4.5, carDefaultAmps: 32},
		{name: "16:30 — home from work", plugInHour: 16.5, carDefaultAmps: 32},
	}

	var dumps []map[string]any

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			got := simulatePlugIn(t, sc, clear, opts, window)

			t.Logf("%s: unsupervised %.1f kWh, solar-matched %.1f kWh, grid %.1f kWh, %d commands (first at %s), SoC 50%%→%.0f%%",
				sc.name, got.unsupervisedWh/1000, got.solarMatchedWh/1000, got.gridWh/1000,
				got.commands, orDash(got.firstCommandAt), got.finalSoc)

			// Deliberately NOT asserting the state-of-charge cap holds.
			//
			// The cap is only enforceable while the loop is running, and the loop only runs
			// 09:00-18:00. A car plugged in at 04:30 reaches 100% on grid power before the
			// controller gets its first say; a car still charging at 18:00 carries on all night
			// at whatever current was last commanded. Both are recorded by the tests below
			// rather than papered over here.
			if got.commands == 0 {
				t.Errorf("%s: the controller never acted at all", sc.name)
			}

			if out := os.Getenv("PLUGIN_OUT"); out != "" {
				dumps = append(dumps, map[string]any{
					"name": sc.name, "plugInHour": sc.plugInHour,
					"unsupervisedWh": got.unsupervisedWh, "solarMatchedWh": got.solarMatchedWh,
					"gridWh": got.gridWh, "commands": got.commands,
					"firstCommandAt": orDash(got.firstCommandAt), "finalSoc": got.finalSoc,
					"steps": got.Steps,
				})
			}
		})
	}

	if out := os.Getenv("PLUGIN_OUT"); out != "" && len(dumps) > 0 {
		blob, err := json.MarshalIndent(dumps, "", " ")
		if err != nil {
			t.Fatalf("marshalling: %v", err)
		}
		if err := os.WriteFile(out, blob, 0o644); err != nil {
			t.Fatalf("writing %s: %v", out, err)
		}
		t.Logf("wrote %d scenarios to %s", len(dumps), out)
	}
}

// Plugging in before dawn must not charge at all until the sun is up. Before the fix the car ran
// at its own 32A on grid power from 04:30 to 09:00 and reached 100%.
func TestPluggingInBeforeDawnWaitsForTheSun(t *testing.T) {
	opts := DefaultChargingOptions()
	window, _ := NewPollingWindow(DefaultPollingWindowOptions())

	var clear weather
	for _, w := range weatherProfiles() {
		if w.name == "clear summer day" {
			clear = w
		}
	}

	got := simulatePlugIn(t,
		plugInScenario{name: "04:30", plugInHour: 4.5, carDefaultAmps: 32},
		clear, opts, window)

	// The car starts itself at 32A on plug-in. In the dark that is pure grid import, so the very
	// first evaluation must stop it — before this fix it ran that way until 09:00.
	if got.drewBeforeWindow {
		t.Error("the car drew current before dawn; only solar should ever charge it")
	}
	if got.unsupervisedWh > 500 {
		t.Errorf("%.0f Wh arrived outside the window; the sunset stop is not holding",
			got.unsupervisedWh)
	}
	if got.firstCommandAt != "04:30" {
		t.Errorf("first command at %s, want 04:30 — the stop should land on the plug-in itself",
			orDash(got.firstCommandAt))
	}

	// It then waits for the sun and charges normally, so the day still ends at the cap.
	if got.solarMatchedWh < 5000 {
		t.Errorf("only %.1f kWh was solar-matched; the car should still charge once the sun is up",
			got.solarMatchedWh/1000)
	}
	if got.finalSoc > float64(opts.MaxSocPercent)+3 {
		t.Errorf("finished at %.0f%%, above the %d%% cap", got.finalSoc, opts.MaxSocPercent)
	}

	t.Logf("before dawn: %.1f kWh unsupervised (was 34.6 before the fix), %.1f kWh solar-matched, "+
		"finishing at %.0f%%", got.unsupervisedWh/1000, got.solarMatchedWh/1000, got.finalSoc)
}

// Arriving late leaves only the tail of the window, and the controller must not chase the sunset
// down below the connector minimum — it stops instead.
func TestPluggingInLateStopsRatherThanTrickling(t *testing.T) {
	opts := DefaultChargingOptions()
	window, _ := NewPollingWindow(DefaultPollingWindowOptions())

	var clear weather
	for _, w := range weatherProfiles() {
		if w.name == "clear summer day" {
			clear = w
		}
	}

	got := simulatePlugIn(t,
		plugInScenario{name: "16:30", plugInHour: 16.5, carDefaultAmps: 32},
		clear, opts, window)

	if got.commands == 0 {
		t.Error("expected at least one command after a 16:30 plug-in inside the window")
	}
	if got.finalSoc <= 50 {
		t.Errorf("SoC did not move from 50%%, got %.1f%%", got.finalSoc)
	}

	if got.finalSoc > float64(opts.MaxSocPercent)+3 {
		t.Errorf("finished at %.0f%%, above the %d%% cap", got.finalSoc, opts.MaxSocPercent)
	}

	t.Logf("16:30 arrival: %d commands, first at %s, SoC 50%%→%.0f%%, of which %.1f kWh arrived "+
		"after the window closed", got.commands, orDash(got.firstCommandAt), got.finalSoc,
		got.unsupervisedWh/1000)
}

func orDash(s string) string {
	if s == "" {
		return "never"
	}
	return s
}

var _ = math.Round
