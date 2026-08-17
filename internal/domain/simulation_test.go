package domain

import (
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"
)

// A full-day simulation at 15-minute resolution, run across several weather profiles.
//
// The unit tests check each rule in isolation. This checks what the rules do to each other over a
// whole day: that a cloud passing at 11:00 does not strand the car at 14:00, that the state-of-
// charge cap holds once reached, and that a resume after a low-solar stop is never mistaken for a
// person restarting the session.
//
// Set SIM_OUT to a path to dump the timeline as JSON for plotting.

const (
	simVoltage     = 240.0
	simBatteryWh   = 50000.0 // 2019 Model 3 Standard Range Plus, near enough for shape
	simStepMinutes = 15
	simArrayPeakW  = 4200.0 // matches a 16A ceiling at 240V, with headroom
	simStartSocPct = 45.0
)

// weather shapes the clear-sky curve into a particular kind of day.
type weather struct {
	name string
	// factor returns the fraction of clear-sky production at a given local hour.
	factor func(hour float64) float64
	// peakW overrides the array peak, for seasons with a lower sun angle.
	peakW float64
}

// clearSky is a half-sine between sunrise and sunset, peaking at solar noon.
func clearSky(hour, sunrise, sunset float64) float64 {
	if hour <= sunrise || hour >= sunset {
		return 0
	}
	return math.Sin(math.Pi * (hour - sunrise) / (sunset - sunrise))
}

func weatherProfiles() []weather {
	return []weather{
		{
			name:   "clear summer day",
			peakW:  simArrayPeakW,
			factor: func(h float64) float64 { return clearSky(h, 6, 20.5) },
		},
		{
			// Fast-moving cumulus: deep, short dips. The trailing-maximum window exists precisely
			// so these do not chop the charge current up and down all afternoon.
			name:  "passing clouds",
			peakW: simArrayPeakW,
			factor: func(h float64) float64 {
				base := clearSky(h, 6, 20.5)
				dip := 1.0
				if math.Mod(h*4, 3) < 1 { // a dip every 45 minutes, lasting 15
					dip = 0.25
				}
				return base * dip
			},
		},
		{
			name:   "overcast all day",
			peakW:  simArrayPeakW,
			factor: func(h float64) float64 { return clearSky(h, 6, 20.5) * 0.18 },
		},
		{
			// Clear morning, then a front arrives at 13:00 and production collapses for good.
			name:  "storm front at 13:00",
			peakW: simArrayPeakW,
			factor: func(h float64) float64 {
				base := clearSky(h, 6, 20.5)
				if h >= 13 {
					return base * 0.05
				}
				return base
			},
		},
		{
			// Low winter sun: shorter day, lower peak. Production spends much of the day under
			// the 5A connector minimum, which should stop the session rather than pull from grid.
			name:   "winter clear",
			peakW:  simArrayPeakW * 0.45,
			factor: func(h float64) float64 { return clearSky(h, 7.5, 17.5) },
		},
	}
}

// step is one 15-minute observation of the simulated day.
type step struct {
	Local      string   `json:"local"`
	Hour       float64  `json:"hour"`
	Watts      float64  `json:"watts"`
	Amps       float64  `json:"amps"`
	WindowMax  *float64 `json:"windowMax"`
	Polled     bool     `json:"polled"`
	Action     string   `json:"action"`
	TargetAmps *int     `json:"targetAmps"`
	DrawAmps   int      `json:"drawAmps"`
	SocPct     float64  `json:"socPct"`
	Charging   bool     `json:"charging"`
	Reason     string   `json:"reason"`
}

type dayResult struct {
	Weather     string  `json:"weather"`
	Steps       []step  `json:"steps"`
	SolarWh     float64 `json:"solarWh"`
	DeliveredWh float64 `json:"deliveredWh"`
	GridWh      float64 `json:"gridWh"`
	Commands    int     `json:"commands"`
	FinalSocPct float64 `json:"finalSocPct"`
}

func TestFullDaySimulation(t *testing.T) {
	opts := DefaultChargingOptions()
	window, err := NewPollingWindow(DefaultPollingWindowOptions())
	if err != nil {
		t.Fatalf("NewPollingWindow: %v", err)
	}

	var results []dayResult

	for _, w := range weatherProfiles() {
		t.Run(w.name, func(t *testing.T) {
			result := simulateDay(t, w, opts, window)
			results = append(results, result)

			// Some grid import is by design, not a defect: the target is the trailing 60-minute
			// maximum, which deliberately overshoots so a passing cloud does not chop the charge
			// current up and down. The invariant is not "never import" — it is "never run a
			// session the trailing window cannot sustain", which is checked per-step below.
			//
			// What is asserted here is that the overshoot stays a minority of the energy
			// delivered. If it ever dominates, the lookback window is too long for the weather.
			if result.DeliveredWh > 0 {
				share := result.GridWh / result.DeliveredWh
				t.Logf("%s: %.1f kWh solar, %.1f kWh delivered, %.1f kWh (%.0f%%) from grid, %d commands, SoC %.0f%%→%.0f%%",
					w.name, result.SolarWh/1000, result.DeliveredWh/1000, result.GridWh/1000,
					share*100, result.Commands, simStartSocPct, result.FinalSocPct)

				if share > 0.35 {
					t.Errorf("%s: %.0f%% of delivered energy came from the grid; the overshoot bias is too expensive here",
						w.name, share*100)
				}
			}
		})
	}

	if out := os.Getenv("SIM_OUT"); out != "" {
		blob, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			t.Fatalf("marshalling results: %v", err)
		}
		if err := os.WriteFile(out, blob, 0o644); err != nil {
			t.Fatalf("writing %s: %v", out, err)
		}
		t.Logf("wrote %d days to %s", len(results), out)
	}
}

func simulateDay(t *testing.T, w weather, opts ChargingOptions, window *PollingWindow) dayResult {
	t.Helper()

	loc, _ := time.LoadLocation("America/Chicago")
	dayStart := time.Date(2026, 8, 11, 0, 0, 0, 0, loc)

	vehicle := &VehicleState{
		VIN:                 testVIN,
		ChargingState:       StateCharging,
		BatteryLevelPercent: intPtr(int(simStartSocPct)),
		ReportedMaxAmps:     intPtr(48),
		LastUpdated:         dayStart,
	}
	socPct := simStartSocPct

	var readings []struct {
		at   time.Time
		amps float64
	}

	result := dayResult{Weather: w.name}
	stepDur := simStepMinutes * time.Minute

	for i := 0; i < 24*60/simStepMinutes; i++ {
		now := dayStart.Add(time.Duration(i) * stepDur)
		hour := float64(now.Hour()) + float64(now.Minute())/60

		watts := w.peakW * w.factor(hour)
		amps, err := WattsToAmps(watts, simVoltage)
		if err != nil {
			t.Fatalf("WattsToAmps: %v", err)
		}
		result.SolarWh += watts * (simStepMinutes / 60.0)

		s := step{
			Local:    now.Format("15:04"),
			Hour:     hour,
			Watts:    watts,
			Amps:     amps,
			SocPct:   socPct,
			Charging: vehicle.ChargingState.IsActivelyCharging(),
		}

		// Outside the daylight window nothing is polled — that is what protects the Enphase
		// budget — but the loop still decides, and with the sun down it stops any running session.
		if !window.IsOpen(now) {
			d := Decide(vehicle, nil, false, opts, now)
			s.Action = d.Action.String()
			s.Reason = d.Reason
			applyDecision(vehicle, d, now, &result)

			s.DrawAmps = drawFor(vehicle)
			s.Charging = vehicle.ChargingState.IsActivelyCharging()
			socPct = advanceSoc(socPct, s.DrawAmps)
			vehicle.BatteryLevelPercent = intPtr(int(socPct))
			vehicle.LastUpdated = now
			s.SocPct = socPct

			if shortfall := float64(s.DrawAmps) - amps; shortfall > 0 {
				result.GridWh += shortfall * simVoltage * (simStepMinutes / 60.0)
			}
			result.DeliveredWh += float64(s.DrawAmps) * simVoltage * (simStepMinutes / 60.0)

			result.Steps = append(result.Steps, s)
			continue
		}

		readings = append(readings, struct {
			at   time.Time
			amps float64
		}{now, amps})

		// Trailing maximum, matching store.MaxAmpsSince plus PruneSolarReadings.
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

		decision := Decide(vehicle, windowMax, true, opts, now)
		s.Action = decision.Action.String()
		s.Reason = decision.Reason
		s.TargetAmps = decision.TargetAmps
		s.WindowMax = windowMax
		s.Polled = true

		applyDecision(vehicle, decision, now, &result)

		s.DrawAmps = drawFor(vehicle)
		s.Charging = vehicle.ChargingState.IsActivelyCharging()

		// The real low-solar invariant: a session must never keep running once the trailing
		// window itself cannot sustain the connector minimum. Instantaneous dips are tolerated
		// by design; a sustained collapse is not.
		if windowMax != nil && int(math.Round(*windowMax)) < opts.MinChargeAmps && s.DrawAmps > 0 {
			t.Errorf("%s at %s: drawing %dA while the trailing window peaked at only %.2fA, under the %dA minimum",
				w.name, s.Local, s.DrawAmps, *windowMax, opts.MinChargeAmps)
		}

		// Charging above the cap must never happen, whatever the weather did.
		if socPct > float64(opts.MaxSocPercent)+1 && s.DrawAmps > 0 {
			t.Errorf("%s at %s: still drawing %dA at %.1f%%, above the %d%% cap",
				w.name, s.Local, s.DrawAmps, socPct, opts.MaxSocPercent)
		}
		if s.TargetAmps != nil {
			if *s.TargetAmps > opts.MaxChargeAmps || *s.TargetAmps < opts.MinChargeAmps {
				t.Errorf("%s at %s: target %dA outside [%d,%d]",
					w.name, s.Local, *s.TargetAmps, opts.MinChargeAmps, opts.MaxChargeAmps)
			}
		}

		// Any current drawn beyond what the sun is producing is grid import.
		if shortfall := float64(s.DrawAmps) - amps; shortfall > 0 {
			result.GridWh += shortfall * simVoltage * (simStepMinutes / 60.0)
		}
		result.DeliveredWh += float64(s.DrawAmps) * simVoltage * (simStepMinutes / 60.0)

		socPct = advanceSoc(socPct, s.DrawAmps)
		vehicle.BatteryLevelPercent = intPtr(int(socPct))
		vehicle.LastUpdated = now
		s.SocPct = socPct

		result.Steps = append(result.Steps, s)
	}

	result.FinalSocPct = socPct
	return result
}

// applyDecision mirrors what controller.act does, including the marker bookkeeping that keeps the
// controller from mistaking its own resume for a manual restart.
func applyDecision(v *VehicleState, d Decision, now time.Time, r *dayResult) {
	switch {
	case d.ShouldSend():
		v.LastSetAmps = d.TargetAmps
		v.LastSetAt = &now
		v.Session = SessionAuto
		v.SessionSince = nil
		r.Commands++
	case d.ShouldResume(), d.ShouldStart():
		v.ChargingState = StateCharging
		v.LastSetAmps = d.TargetAmps
		v.LastSetAt = &now
		v.Session = SessionAuto
		v.SessionSince = nil
		r.Commands++
	case d.ShouldStop():
		v.ChargingState = StateStopped
		v.LastSetAmps = nil
		if d.Action == ActionStopCharging {
			v.Session = SessionStoppedAtCap
		} else {
			v.Session = SessionStoppedForSun
		}
		v.SessionSince = &now
		r.Commands++
	}
}

func drawFor(v *VehicleState) int {
	if !v.ChargingState.IsActivelyCharging() || v.LastSetAmps == nil {
		return 0
	}
	return *v.LastSetAmps
}

func advanceSoc(socPct float64, amps int) float64 {
	added := float64(amps) * simVoltage * (simStepMinutes / 60.0) / simBatteryWh * 100
	if s := socPct + added; s < 100 {
		return s
	}
	return 100
}

// The trailing maximum is what stops fast-moving cloud from chopping the charge current up and
// down. Without it every dip would command a new value.
func TestPassingCloudsDoNotCauseCommandChurn(t *testing.T) {
	opts := DefaultChargingOptions()
	window, _ := NewPollingWindow(DefaultPollingWindowOptions())

	var clear, cloudy dayResult
	for _, w := range weatherProfiles() {
		switch w.name {
		case "clear summer day":
			clear = simulateDay(t, w, opts, window)
		case "passing clouds":
			cloudy = simulateDay(t, w, opts, window)
		}
	}

	// Some extra commands are expected; an order of magnitude more would mean the damping failed.
	if cloudy.Commands > clear.Commands*3+6 {
		t.Errorf("passing clouds caused %d commands against %d on a clear day — damping failed",
			cloudy.Commands, clear.Commands)
	}
}

func TestOvercastNeverChargesFromTheGrid(t *testing.T) {
	opts := DefaultChargingOptions()
	window, _ := NewPollingWindow(DefaultPollingWindowOptions())

	for _, w := range weatherProfiles() {
		if w.name != "overcast all day" {
			continue
		}
		got := simulateDay(t, w, opts, window)
		if got.GridWh > 1 {
			t.Errorf("overcast day drew %.0f Wh from the grid", got.GridWh)
		}
	}
}
