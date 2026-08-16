// Package controller runs the solar-to-amps control loop and the telemetry ingest that feeds it.
//
// This replaces the two Azure Functions the project used to be: a timer trigger and an
// HTTP-triggered ingest endpoint. On a single box neither needs a host — the loop is a ticker and
// the ingest is a goroutine reading the same in-process store.
package controller

import (
	"context"
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

// Commander sends signed commands to a vehicle.
type Commander interface {
	SetChargingAmps(ctx context.Context, vin string, amps int) error
	StopCharging(ctx context.Context, vin string) error
	StartCharging(ctx context.Context, vin string, amps int) error
	Wake(ctx context.Context, vin string) error
}

// Store is the persistence the loop and the ingest share.
type Store interface {
	GetVehicleState(ctx context.Context, vin string) (*domain.VehicleState, error)
	LatestVehicleState(ctx context.Context) (*domain.VehicleState, error)
	SaveVehicleState(ctx context.Context, v *domain.VehicleState) error
	AddSolarReading(ctx context.Context, at time.Time, watts, amps float64) error
	MaxAmpsSince(ctx context.Context, since time.Time) (*float64, error)
	ReadingsAboveSince(ctx context.Context, since time.Time, minAmps float64) (int, error)
	RecordWake(ctx context.Context, vin string, at time.Time) error
	WakesSince(ctx context.Context, vin string, since time.Time) (int, error)
}

type Controller struct {
	store    Store
	solar    SolarReader
	commands Commander
	window   *domain.PollingWindow
	opts     domain.ChargingOptions
	log      *slog.Logger

	// One mutex over both paths. Telemetry and the control loop both read-modify-write the same
	// vehicle record, and without this a frame arriving mid-evaluation could overwrite the
	// LastSetAmps we just recorded — which would then look like a manual override.
	mu sync.Mutex
}

func New(store Store, solar SolarReader, commands Commander, window *domain.PollingWindow,
	opts domain.ChargingOptions, log *slog.Logger) *Controller {
	return &Controller{store: store, solar: solar, commands: commands, window: window, opts: opts, log: log}
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
	conn, err := telemetry.DecodeConnectivity(raw, time.Now().UTC())
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	state, err := c.store.GetVehicleState(ctx, conn.VIN)
	if err != nil {
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

	return c.store.SaveVehicleState(ctx, state)
}

// HandleTelemetry decodes one vehicle-data record and folds it into the vehicle's state.
func (c *Controller) HandleTelemetry(ctx context.Context, raw []byte) error {
	obs, err := telemetry.DecodeBytes(raw, time.Now().UTC())
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	state, err := c.store.GetVehicleState(ctx, obs.VIN)
	if err != nil {
		return err
	}
	if state == nil {
		state = domain.NewVehicleState(obs.VIN, obs.ObservedAt)
	}

	before := state.OverrideActive
	state = domain.ApplyObservation(state, obs, c.opts)

	if state.OverrideActive && !before {
		c.log.Warn("manual override detected; automatic adjustment stops until the car unplugs",
			"vin", obs.VIN, "reported_amps", deref(obs.ReportedAmps), "last_set", deref(state.LastSetAmps))
	}

	return c.store.SaveVehicleState(ctx, state)
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

// Evaluate runs one cycle: poll solar, decide, act.
//
// Exported so it can be driven directly in tests without waiting on a ticker.
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

	c.mu.Lock()
	defer c.mu.Unlock()

	maxAmps, err := c.store.MaxAmpsSince(ctx, now.Add(-c.opts.LookbackWindow))
	if err != nil {
		return err
	}

	vehicle, err := c.store.LatestVehicleState(ctx)
	if err != nil {
		return err
	}

	decision := domain.Decide(vehicle, maxAmps, c.window.IsOpen(now), c.opts, now)
	c.log.Info("decision", "action", decision.Action.String(), "reason", decision.Reason)

	// A sleeping car cannot be commanded, so the decision above can only ever skip. Waking is the
	// separate question of whether that is worth $0.02 and a battery-draining wake window.
	if decision.Action == domain.ActionSkipNotCharging && c.opts.WakeToCharge {
		if err := c.considerWake(ctx, vehicle, maxAmps, now); err != nil {
			c.log.Error("wake failed", "error", err)
		}
		return nil
	}

	return c.act(ctx, vehicle, decision, now)
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
		// Any earlier stop is now history; leaving the marker set would make the next telemetry
		// frame look like a person restarting the session.
		vehicle.LowSolarStopIssuedAt = nil

	case d.ShouldResume(), d.ShouldStart():
		if err := c.commands.StartCharging(ctx, vehicle.VIN, *d.TargetAmps); err != nil {
			return err
		}
		vehicle.LastSetAmps = d.TargetAmps
		vehicle.LastSetAt = &now
		// Cleared before the car can report Charging again, so our own resume is never mistaken
		// for a manual restart.
		vehicle.LowSolarStopIssuedAt = nil

	case d.ShouldStop():
		if err := c.commands.StopCharging(ctx, vehicle.VIN); err != nil {
			return err
		}
		if d.Action == domain.ActionStopCharging {
			vehicle.SocStopIssuedAt = &now
		} else {
			vehicle.LowSolarStopIssuedAt = &now
		}
		vehicle.LastSetAmps = nil

	default:
		return nil // A skip changes no state.
	}

	return c.store.SaveVehicleState(ctx, vehicle)
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
