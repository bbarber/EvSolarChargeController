// Package controller runs the solar-to-amps control loop and the telemetry ingest that feeds it.
//
// This replaces the two Azure Functions the project used to be: a timer trigger and an
// HTTP-triggered ingest endpoint. On a single box neither needs a host — the loop is a ticker and
// the ingest is a goroutine reading the same in-process store.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bbarber/EvSolarChargeController/internal/domain"
	"github.com/bbarber/EvSolarChargeController/internal/enphase"
	"github.com/bbarber/EvSolarChargeController/internal/telemetry"
)

// TickMinutes are the minutes past the hour the loop evaluates on: three times an hour, matching
// the Enphase budget arithmetic of 3 calls/hour x 9 hours x 31 days = 837 against a 1000 cap.
var TickMinutes = []int{0, 20, 40}

// SolarReader is the subset of the Enphase client the loop needs.
type SolarReader interface {
	CurrentProduction(ctx context.Context, now time.Time) enphase.Result
}

// Recorder receives copies of what happened, for the dashboard. Nil-safe: a nil recorder means
// no mirroring, and no Record call may ever fail the caller.
type Recorder interface {
	RecordStatus(ctx context.Context, v *domain.VehicleState)
	RecordSolar(ctx context.Context, at time.Time, watts, amps float64)
	RecordCharge(ctx context.Context, at time.Time, vin string, amps int, watts float64)
	RecordEvent(ctx context.Context, at time.Time, vin, kind, action, reason string)
}

// Commander sends signed commands to a vehicle.
type Commander interface {
	SetChargingAmps(ctx context.Context, vin string, amps int) error
	StopCharging(ctx context.Context, vin string) error
	StartCharging(ctx context.Context, vin string, amps int) error
	Wake(ctx context.Context, vin string) error
	Location(ctx context.Context, vin string) (lat, lon float64, err error)
}

// Store is the persistence the loop and the ingest share.
type Store interface {
	GetVehicleState(ctx context.Context, vin string) (*domain.VehicleState, error)
	SaveVehicleState(ctx context.Context, v *domain.VehicleState) error
	AddSolarReading(ctx context.Context, at time.Time, watts, amps float64) error
	MaxAmpsSince(ctx context.Context, since time.Time) (*float64, error)
	ReadingsAboveSince(ctx context.Context, since time.Time, minAmps float64) (int, error)
	RecordWake(ctx context.Context, vin string, at time.Time) error
	WakesSince(ctx context.Context, vin string, since time.Time) (int, error)
}

type Controller struct {
	vins     []string
	store    Store
	solar    SolarReader
	commands Commander
	window   *domain.PollingWindow
	opts     domain.ChargingOptions
	log      *slog.Logger
	recorder Recorder // may be nil

	// now is the clock, replaceable in tests. Event-driven paths need a time and must not
	// invent one per call site.
	now func() time.Time

	// positionAsked is when a position read was last attempted for each VIN, enforcing the
	// cooldown. Deliberately in memory: a restart may retry once, which costs a single read and
	// is better than a cooldown surviving the deploy that was meant to fix position handling.
	positionAsked map[string]time.Time

	// One mutex over both paths. Telemetry and the control loop both read-modify-write the same
	// vehicle record, and without this a frame arriving mid-evaluation could overwrite the
	// LastSetAmps we just recorded — which would then look like a manual override.
	mu sync.Mutex
}

func New(vins []string, store Store, solar SolarReader, commands Commander, window *domain.PollingWindow,
	opts domain.ChargingOptions, log *slog.Logger) *Controller {
	return &Controller{vins: vins, store: store, solar: solar, commands: commands, window: window,
		opts: opts, log: log, positionAsked: make(map[string]time.Time),
		now: func() time.Time { return time.Now().UTC() }}
}

// SetRecorder attaches the dashboard mirror. Optional; nil disables mirroring.
func (c *Controller) SetRecorder(r Recorder) { c.recorder = r }

func (c *Controller) record(fn func(Recorder)) {
	if c.recorder != nil {
		fn(c.recorder)
	}
}

// SetChargingOptions replaces the charging options. Used by tests to exercise a mode without
// standing up a second controller.
func (c *Controller) SetChargingOptions(opts domain.ChargingOptions) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opts = opts
}

// HandleRecord routes one fleet-telemetry record by its topic.
//
// Both record types arrive on the same socket, because a ZMQ SUB filter is a prefix match and the
// subscription is the bare namespace.
func (c *Controller) HandleRecord(ctx context.Context, topic string, raw []byte) error {
	switch telemetry.RecordTypeFromTopic(topic) {
	case telemetry.RecordConnectivity:
		return c.handleConnectivity(ctx, raw)
	case telemetry.RecordVehicleData:
		return c.HandleTelemetry(ctx, raw)
	default:
		return nil // A record type we do not subscribe to; ignore rather than error.
	}
}

// handleConnectivity records whether the car is reachable. This is the only trustworthy answer to
// that question: a connected but parked car sends no data, so data age cannot distinguish idle
// from asleep.
func (c *Controller) handleConnectivity(ctx context.Context, raw []byte) error {
	conn, err := telemetry.DecodeConnectivity(raw, c.now())
	if err != nil {
		return err
	}

	c.mu.Lock()
	state, err := c.store.GetVehicleState(ctx, conn.VIN)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if state == nil {
		state = domain.NewVehicleState(conn.VIN, conn.ObservedAt)
	}

	online := conn.Online
	state.Online = &online
	state.OnlineAt = &conn.ObservedAt

	c.log.Info("vehicle connectivity changed",
		"vin", conn.VIN, "online", conn.Online, "network", conn.Network, "at", conn.ObservedAt)

	if err := c.store.SaveVehicleState(ctx, state); err != nil {
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()

	c.record(func(r Recorder) {
		online := "offline"
		if conn.Online {
			online = "online"
		}
		r.RecordEvent(ctx, conn.ObservedAt, conn.VIN, "connectivity", online, "network "+conn.Network)
		r.RecordStatus(ctx, state)
	})

	// Evaluate on the event, not the next tick. This is the moment that matters most: a car that
	// just came online after our wake is awake NOW, and twenty minutes from now it will not be.
	// Today's first wake was wasted exactly this way.
	if err := c.evaluate(ctx, c.now()); err != nil {
		c.log.Error("evaluation after connectivity event failed", "error", err)
	}
	return nil
}

// HandleTelemetry decodes one vehicle-data record and folds it into the vehicle's state.
func (c *Controller) HandleTelemetry(ctx context.Context, raw []byte) error {
	obs, err := telemetry.DecodeBytes(raw, c.now())
	if err != nil {
		return err
	}

	c.mu.Lock()
	state, err := c.store.GetVehicleState(ctx, obs.VIN)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if state == nil {
		state = domain.NewVehicleState(obs.VIN, obs.ObservedAt)
	}

	before := state.Session
	state = domain.ApplyObservation(state, obs, c.opts)

	if state.Session == domain.SessionOverridden && before != domain.SessionOverridden {
		c.log.Warn("manual override detected; automatic adjustment stops until the car unplugs",
			"vin", obs.VIN, "reported_amps", deref(obs.ReportedAmps), "last_set", deref(state.LastSetAmps))
	}

	if err := c.store.SaveVehicleState(ctx, state); err != nil {
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()

	c.record(func(r Recorder) {
		r.RecordStatus(ctx, state)
		// One draw sample per frame. A charging car reports every minute or so; a stopped one
		// only on transitions, so the zero that closes the step arrives exactly once.
		draw := 0
		if state.ChargingState.IsActivelyCharging() && state.ChargeAmps != nil {
			draw = *state.ChargeAmps
		}
		r.RecordCharge(ctx, obs.ObservedAt, obs.VIN, draw, float64(draw)*c.opts.SystemVoltage)
	})

	// Evaluate on the event. A plug-in acts within seconds instead of waiting out the tick, and a
	// frame that tripped the override stops us commanding on stale intent. Decisions are free —
	// only the Enphase poll costs anything, and that stays on the clock.
	if err := c.evaluate(ctx, c.now()); err != nil {
		c.log.Error("evaluation after telemetry event failed", "error", err)
	}
	return nil
}

// Run evaluates on every tick until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) error {
	c.log.Info("control loop started",
		"window", c.window.Describe(time.Now()), "ticks", TickMinutes)

	for {
		next := NextTick(time.Now())
		timer := time.NewTimer(time.Until(next))

		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case now := <-timer.C:
			if err := c.Evaluate(ctx, now.UTC()); err != nil {
				c.log.Error("evaluation failed", "error", err)
			}
		}
	}
}

// Evaluate is one scheduled cycle: poll solar if the window is open, then decide and act.
//
// The clock exists for the poll. Deciding also happens here so a quiet system still reconsiders
// every twenty minutes, but events do not wait for it — telemetry and connectivity evaluate the
// moment they arrive.
func (c *Controller) Evaluate(ctx context.Context, now time.Time) error {
	// Only the *poll* is window-bound. The decision runs around the clock.
	//
	// These were conflated originally, and it cost both the state-of-charge cap and the premise of
	// the whole system: a car plugged in at 04:30 charged to 100% on grid power before the loop
	// first ran at 09:00, and a car still charging at 18:00 carried on all night. The Enphase plan
	// is what the window protects — 1000 calls a month — and deciding costs nothing against it.
	//
	// Outside the window Decide stops any running session: this car charges on solar or not at
	// all. It is recorded as a low-solar stop, so it resumes by itself the next morning.
	if c.window.IsOpen(now) {
		// Poll first, and record whatever came back, before taking any lock: the HTTP call is the
		// slow part and holding the mutex across it would stall telemetry ingest.
		result := c.solar.CurrentProduction(ctx, now)
		if result.Success() {
			amps, err := domain.WattsToAmps(result.Production.Watts, c.opts.SystemVoltage)
			if err != nil {
				return err
			}
			if err := c.store.AddSolarReading(ctx, result.Production.ReadingAt, result.Production.Watts, amps); err != nil {
				return err
			}
			c.log.Info("solar reading recorded",
				"watts", result.Production.Watts, "amps", amps, "at", result.Production.ReadingAt)
			c.record(func(r Recorder) {
				r.RecordSolar(ctx, result.Production.ReadingAt, result.Production.Watts, amps)
			})
		} else {
			// A failed poll is not evidence of low production, so the cycle continues on whatever
			// readings are still inside the window.
			c.log.Warn("solar poll produced no reading",
				"reason", result.Reason, "message", result.Message)
		}
	} else {
		c.log.Debug("outside the polling window; deciding without spending an Enphase call",
			"at", c.window.Describe(now))
	}

	// Readings are never deleted. At 27 a day a year of history is about a megabyte against a
	// 47 GB volume, reading_at is the primary key so range scans stay cheap at any realistic size,
	// and the accumulated record is the only real measurement of this array that exists — the
	// simulations still run on an invented 4.2 kW peak. Pruning bought nothing and, when its
	// horizon was shared with the wake gate, silently broke it.

	return c.evaluate(ctx, now)
}

// evaluate decides and acts for every managed vehicle. Shared by the tick and by every event.
//
// Each car is judged on its own state. The previous approach acted on whichever car reported most
// recently, and a second car being driven around town — battery level changes while driving —
// out-shouts one sitting plugged in at home.
func (c *Controller) evaluate(ctx context.Context, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	maxAmps, err := c.store.MaxAmpsSince(ctx, now.Add(-c.opts.LookbackWindow))
	if err != nil {
		return err
	}

	for _, vin := range c.vins {
		vehicle, err := c.store.GetVehicleState(ctx, vin)
		if err != nil {
			return err
		}
		if vehicle == nil {
			continue // Never reported; nothing to decide about.
		}

		// Settle where the car is before asking what to do about it: the home gate can only skip
		// while the answer is stale, and a skip looks exactly like a car that is genuinely away.
		c.resolvePosition(ctx, vehicle, now)

		decision := domain.Decide(vehicle, maxAmps, c.window.IsOpen(now), c.opts, now)
		c.log.Info("decision", "vin", vin,
			"action", decision.Action.String(), "reason", decision.Reason)

		// Mirror decisions that say something happened or changed; the every-few-minutes quiet
		// skips would bury the interesting rows.
		switch decision.Action {
		case domain.ActionSkipInsufficientSolar, domain.ActionSkipNoSolarData,
			domain.ActionSkipNotCharging, domain.ActionSkipAlreadyAtTarget,
			domain.ActionSkipNotAtHome, domain.ActionSkipAtSocCap, domain.ActionSkipNoVehicleState:
		default:
			c.record(func(r Recorder) {
				r.RecordEvent(ctx, now, vin, "decision", decision.Action.String(), decision.Reason)
			})
		}

		// A sleeping car cannot be commanded, so the decision above can only ever skip. Waking is
		// the separate question of whether that is worth $0.02 and a wake window.
		if decision.Action == domain.ActionSkipNotCharging && c.opts.WakeToCharge {
			if err := c.considerWake(ctx, vehicle, maxAmps, now); err != nil {
				c.log.Error("wake failed", "vin", vin, "error", err)
			}
			continue
		}

		if err := c.act(ctx, vehicle, decision, now); err != nil {
			return err
		}
	}
	return nil
}

// resolvePosition asks the car where it is when the stored answer is missing or stale, and folds
// the result into the vehicle record. Mutates vehicle in place.
//
// Failure is never fatal: a car that dozed off between the connectivity event and the call returns
// HTTP 408, which is an ordinary outcome. The stale answer stands and the cooldown keeps the next
// attempt from following immediately.
func (c *Controller) resolvePosition(ctx context.Context, vehicle *domain.VehicleState, now time.Time) {
	if vehicle == nil {
		return
	}

	var lastAttempt *time.Time
	if at, ok := c.positionAsked[vehicle.VIN]; ok {
		lastAttempt = &at
	}

	decision := domain.DecidePositionFix(vehicle, lastAttempt, c.opts, now)
	if !decision.Resolve {
		c.log.Debug("position not resolved", "vin", vehicle.VIN, "reason", decision.Reason)
		return
	}

	c.log.Info("resolving position", "vin", vehicle.VIN, "reason", decision.Reason)
	c.positionAsked[vehicle.VIN] = now

	lat, lon, err := c.commands.Location(ctx, vehicle.VIN)
	if err != nil {
		c.log.Warn("position read failed", "vin", vehicle.VIN, "error", err)
		c.record(func(r Recorder) {
			r.RecordEvent(ctx, now, vehicle.VIN, "error", "PositionReadFailed", err.Error())
		})
		return
	}

	domain.ApplyPositionFix(vehicle, lat, lon, now, c.opts)
	if err := c.store.SaveVehicleState(ctx, vehicle); err != nil {
		c.log.Error("saving the resolved position", "vin", vehicle.VIN, "error", err)
		return
	}

	// The distance is logged; the coordinate is not, and never reaches the database.
	atHome := vehicle.AtHome != nil && *vehicle.AtHome
	meters := domain.MetersFromHome(lat, lon, c.opts)
	c.log.Info("position resolved", "vin", vehicle.VIN,
		"at_home", atHome, "meters_from_home", int(meters))
	c.record(func(r Recorder) {
		r.RecordEvent(ctx, now, vehicle.VIN, "decision", "PositionResolved",
			fmt.Sprintf("%s is %dm from home; at_home=%t.", vehicle.VIN, int(meters), atHome))
	})
}

// considerWake evaluates every wake gate and, if they all pass, wakes the car. It does not charge:
// the next tick sees an online car and takes it from there, which keeps the wake decision and the
// charging decision independent.
func (c *Controller) considerWake(ctx context.Context, vehicle *domain.VehicleState, maxAmps *float64, now time.Time) error {
	if vehicle == nil {
		return nil
	}

	local := c.window.ToLocal(now)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location())

	wakesToday, err := c.store.WakesSince(ctx, vehicle.VIN, midnight)
	if err != nil {
		return err
	}

	above, err := c.store.ReadingsAboveSince(ctx,
		now.Add(-c.opts.SustainedWindow), float64(c.opts.MinChargeAmps)-0.5)
	if err != nil {
		return err
	}

	d := domain.DecideWake(vehicle, domain.WakeInputs{
		MaxAmpsLastWindow:    maxAmps,
		ReadingsAboveMinimum: above,
		WakesToday:           wakesToday,
		LocalNow:             local,
	}, c.opts)

	if !d.Wake {
		c.log.Debug("not waking", "reason", d.Reason)
		return nil
	}

	c.log.Info("waking the vehicle", "vin", vehicle.VIN, "reason", d.Reason,
		"wakes_today", wakesToday, "limit", c.opts.MaxWakesPerDay)

	if err := c.commands.Wake(ctx, vehicle.VIN); err != nil {
		return err
	}
	c.record(func(r Recorder) { r.RecordEvent(ctx, now, vehicle.VIN, "wake", "wake_up", d.Reason) })

	// Recorded only on success, so a failed wake does not consume the daily allowance — but the
	// cooldown is set either way by the caller retrying no sooner than the next tick.
	if err := c.store.RecordWake(ctx, vehicle.VIN, now); err != nil {
		return err
	}
	vehicle.LastWakeAt = &now
	return c.store.SaveVehicleState(ctx, vehicle)
}

func (c *Controller) act(ctx context.Context, vehicle *domain.VehicleState, d domain.Decision, now time.Time) error {
	if vehicle == nil {
		return nil // Nothing to command and nothing to record.
	}

	switch {
	case d.ShouldSend():
		if err := c.commands.SetChargingAmps(ctx, vehicle.VIN, *d.TargetAmps); err != nil {
			return err
		}
		vehicle.LastSetAmps = d.TargetAmps
		vehicle.LastSetAt = &now
		// Any earlier stop is history: a StoppedByUs session with a charging car reads as a
		// person restarting it, and this charging is our own doing.
		vehicle.Session = domain.SessionAuto
		vehicle.SessionSince = nil

	case d.ShouldResume(), d.ShouldStart():
		if err := c.commands.StartCharging(ctx, vehicle.VIN, *d.TargetAmps); err != nil {
			return err
		}
		vehicle.LastSetAmps = d.TargetAmps
		vehicle.LastSetAt = &now
		// Back to Auto before the car can report Charging again, so our own resume is never
		// mistaken for a manual restart.
		vehicle.Session = domain.SessionAuto
		vehicle.SessionSince = nil

	case d.ShouldStop():
		if err := c.commands.StopCharging(ctx, vehicle.VIN); err != nil {
			return err
		}
		if d.Action == domain.ActionStopCharging {
			vehicle.Session = domain.SessionStoppedAtCap
		} else {
			vehicle.Session = domain.SessionStoppedForSun
		}
		vehicle.SessionSince = &now
		vehicle.LastSetAmps = nil

	default:
		return nil // A skip changes no state.
	}

	if err := c.store.SaveVehicleState(ctx, vehicle); err != nil {
		return err
	}
	c.record(func(r Recorder) {
		action := d.Action.String()
		amps := ""
		if d.TargetAmps != nil {
			amps = fmt.Sprintf(" %dA", *d.TargetAmps)
		}
		r.RecordEvent(ctx, now, vehicle.VIN, "command", action+amps, d.Reason)
		r.RecordStatus(ctx, vehicle)
	})
	return nil
}

// NextTick returns the next scheduled evaluation at or after now.
func NextTick(now time.Time) time.Time {
	base := now.Truncate(time.Minute)
	for offset := 0; offset <= 60; offset++ {
		candidate := base.Add(time.Duration(offset) * time.Minute)
		if !candidate.After(now) {
			continue
		}
		for _, m := range TickMinutes {
			if candidate.Minute() == m {
				return candidate
			}
		}
	}
	return now.Add(time.Minute)
}

func deref(v *int) any {
	if v == nil {
		return "nil"
	}
	return *v
}
