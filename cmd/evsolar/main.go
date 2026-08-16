// Command evsolar matches a Tesla's charge current to rooftop solar production.
//
// It runs two things concurrently: a telemetry ingest that folds pushed vehicle records into local
// state, and a control loop that polls solar on a schedule and issues signed charging commands.
// The vehicle is never polled — polling can wake a sleeping car, so every piece of vehicle state
// arrives by push.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bbarber/EvSolarChargeController/internal/config"
	"github.com/bbarber/EvSolarChargeController/internal/controller"
	"github.com/bbarber/EvSolarChargeController/internal/domain"
	"github.com/bbarber/EvSolarChargeController/internal/enphase"
	"github.com/bbarber/EvSolarChargeController/internal/store"
	"github.com/bbarber/EvSolarChargeController/internal/telemetry"
	"github.com/bbarber/EvSolarChargeController/internal/tesla"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := newLogger(cfg.LogLevel)

	db, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := seedTokens(ctx, db, cfg, log); err != nil {
		return err
	}

	solar := enphase.New(cfg.Enphase, db, db, log.With("component", "enphase"))

	commander, err := tesla.New(cfg.Tesla, db, log.With("component", "tesla"))
	if err != nil {
		return err
	}

	window, err := domain.NewPollingWindow(cfg.Window)
	if err != nil {
		return err
	}

	ctrl := controller.New(db, solar, commander, window, cfg.Charging, log.With("component", "controller"))
	subscriber := telemetry.NewSubscriber(cfg.ZMQEndpoint, cfg.ZMQTopic, log.With("component", "telemetry"))

	log.Info("starting",
		"vins", cfg.VINs,
		"window", window.Describe(time.Now()),
		"voltage", cfg.Charging.SystemVoltage,
		"amps", fmt.Sprintf("%d-%d", cfg.Charging.MinChargeAmps, cfg.Charging.MaxChargeAmps),
		"soc_cap", cfg.Charging.MaxSocPercent,
		"start_when_plugged_in", cfg.Charging.StartWhenPluggedIn,
		"enphase_budget", cfg.Enphase.MonthlyCallBudget,
		"command_key", commander.KeyFingerprint(),
		"database", cfg.DatabasePath)

	var wg sync.WaitGroup
	errs := make(chan error, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		// Telemetry is the only source of vehicle state, so losing it is fatal rather than
		// something to limp along without: the control loop would go on deciding from a snapshot
		// that quietly ages out and then reads as "asleep" forever.
		if err := subscriber.Run(ctx, ctrl.HandleTelemetry); err != nil {
			errs <- fmt.Errorf("telemetry ingest: %w", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := ctrl.Run(ctx); err != nil {
			errs <- fmt.Errorf("control loop: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-errs:
		log.Error("component failed", "error", err)
		stop()
		wg.Wait()
		return err
	}

	wg.Wait()
	log.Info("stopped cleanly")
	return nil
}

// seedTokens writes the environment-supplied refresh tokens only when the store has none.
//
// Both providers rotate their refresh token on every use, so once a refresh has happened the stored
// copy is the only valid one. Re-seeding from a stale environment variable on every restart would
// hand back an already-consumed token and lock the integration out until someone re-authorizes.
func seedTokens(ctx context.Context, db *store.Store, cfg config.Config, log *slog.Logger) error {
	seeds := map[string]string{
		cfg.Enphase.RefreshTokenSecretName: cfg.EnphaseRefreshTokenSeed,
		cfg.Tesla.RefreshTokenSecretName:   cfg.TeslaRefreshTokenSeed,
	}

	for name, seed := range seeds {
		_, err := db.GetSecret(ctx, name)
		if err == nil {
			continue // Already stored, and possibly already rotated. Leave it alone.
		}
		if !errors.Is(err, store.ErrSecretNotFound) {
			return err
		}
		if seed == "" {
			return fmt.Errorf("no %s in the database and none supplied to seed it; see docs/SETUP.md", name)
		}
		if err := db.PutSecret(ctx, name, seed); err != nil {
			return err
		}
		log.Info("seeded a refresh token from the environment", "name", name)
	}
	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
