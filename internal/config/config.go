// Package config reads the controller's settings from the environment.
//
// Environment rather than a file because the only deployment is a container: docker compose already
// owns an env file, and adding a config format would mean two places to look.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bbarber/EvSolarChargeController/internal/domain"
	"github.com/bbarber/EvSolarChargeController/internal/enphase"
	"github.com/bbarber/EvSolarChargeController/internal/tesla"
)

type Config struct {
	DatabasePath string

	ZMQEndpoint string
	ZMQTopic    string

	VINs []string

	Charging domain.ChargingOptions
	Window   domain.PollingWindowOptions
	Enphase  enphase.Options
	Tesla    tesla.Options

	// Seed values are used only if the store holds no token yet. After the first refresh the
	// stored copy is authoritative, because both providers rotate the token on every use.
	EnphaseRefreshTokenSeed string
	TeslaRefreshTokenSeed   string

	LogLevel string
}

// Load reads configuration and validates it. Every failure is reported at once rather than one per
// restart, because a container that dies on the first missing variable makes for a slow setup.
func Load() (Config, error) {
	cfg := Config{
		DatabasePath: env("EVSOLAR_DB_PATH", "/var/lib/evsolar/evsolar.db"),
		ZMQEndpoint:  env("EVSOLAR_ZMQ_ENDPOINT", "tcp://127.0.0.1:5284"),
		ZMQTopic:     env("EVSOLAR_ZMQ_TOPIC", "V"),
		Charging:     domain.DefaultChargingOptions(),
		Window:       domain.DefaultPollingWindowOptions(),
		Enphase:      enphase.DefaultOptions(),
		Tesla:        tesla.DefaultOptions(),
		LogLevel:     env("EVSOLAR_LOG_LEVEL", "info"),

		EnphaseRefreshTokenSeed: os.Getenv("EVSOLAR_ENPHASE_REFRESH_TOKEN"),
		TeslaRefreshTokenSeed:   os.Getenv("EVSOLAR_TESLA_REFRESH_TOKEN"),
	}

	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if raw := os.Getenv("EVSOLAR_VINS"); raw != "" {
		for _, vin := range strings.Split(raw, ",") {
			if trimmed := strings.TrimSpace(vin); trimmed != "" {
				cfg.VINs = append(cfg.VINs, trimmed)
			}
		}
	}
	if len(cfg.VINs) == 0 {
		fail("EVSOLAR_VINS is required (comma-separated)")
	}

	cfg.Charging.SystemVoltage = envFloat("EVSOLAR_SYSTEM_VOLTAGE", cfg.Charging.SystemVoltage, fail)
	cfg.Charging.MinChargeAmps = envInt("EVSOLAR_MIN_AMPS", cfg.Charging.MinChargeAmps, fail)
	cfg.Charging.MaxChargeAmps = envInt("EVSOLAR_MAX_AMPS", cfg.Charging.MaxChargeAmps, fail)
	cfg.Charging.MaxSocPercent = envInt("EVSOLAR_MAX_SOC_PERCENT", cfg.Charging.MaxSocPercent, fail)
	cfg.Charging.LookbackWindow = envDuration("EVSOLAR_LOOKBACK", cfg.Charging.LookbackWindow, fail)
	cfg.Charging.OverrideSettleWindow = envDuration("EVSOLAR_OVERRIDE_SETTLE", cfg.Charging.OverrideSettleWindow, fail)
	cfg.Charging.VehicleStateStaleAfter = envDuration("EVSOLAR_STATE_STALE_AFTER", cfg.Charging.VehicleStateStaleAfter, fail)
	cfg.Charging.StartWhenPluggedIn = envBool("EVSOLAR_START_WHEN_PLUGGED_IN", cfg.Charging.StartWhenPluggedIn, fail)

	cfg.Window.TimeZone = env("EVSOLAR_TIMEZONE", cfg.Window.TimeZone)
	cfg.Window.StartHourLocal = envInt("EVSOLAR_WINDOW_START_HOUR", cfg.Window.StartHourLocal, fail)
	cfg.Window.EndHourLocal = envInt("EVSOLAR_WINDOW_END_HOUR", cfg.Window.EndHourLocal, fail)

	cfg.Enphase.ClientID = required("EVSOLAR_ENPHASE_CLIENT_ID", fail)
	cfg.Enphase.ClientSecret = required("EVSOLAR_ENPHASE_CLIENT_SECRET", fail)
	cfg.Enphase.APIKey = required("EVSOLAR_ENPHASE_API_KEY", fail)
	cfg.Enphase.SystemID = required("EVSOLAR_ENPHASE_SYSTEM_ID", fail)
	cfg.Enphase.MonthlyCallBudget = envInt("EVSOLAR_ENPHASE_BUDGET", cfg.Enphase.MonthlyCallBudget, fail)

	cfg.Tesla.ClientID = required("EVSOLAR_TESLA_CLIENT_ID", fail)
	cfg.Tesla.PrivateKeyPath = env("EVSOLAR_TESLA_KEY_PATH", "/etc/evsolar/fleet-key.pem")
	cfg.Tesla.SessionCachePath = env("EVSOLAR_TESLA_SESSION_CACHE", "/var/lib/evsolar/sessions.json")

	if cfg.Charging.MinChargeAmps > cfg.Charging.MaxChargeAmps {
		fail("EVSOLAR_MIN_AMPS (%d) cannot exceed EVSOLAR_MAX_AMPS (%d)",
			cfg.Charging.MinChargeAmps, cfg.Charging.MaxChargeAmps)
	}
	if cfg.Window.StartHourLocal >= cfg.Window.EndHourLocal {
		fail("EVSOLAR_WINDOW_START_HOUR (%d) must be before EVSOLAR_WINDOW_END_HOUR (%d)",
			cfg.Window.StartHourLocal, cfg.Window.EndHourLocal)
	}
	if _, err := time.LoadLocation(cfg.Window.TimeZone); err != nil {
		fail("EVSOLAR_TIMEZONE %q is not a known IANA zone: %v", cfg.Window.TimeZone, err)
	}

	if len(problems) > 0 {
		return Config{}, fmt.Errorf("configuration invalid:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func required(key string, fail func(string, ...any)) string {
	v := os.Getenv(key)
	if v == "" {
		fail("%s is required", key)
	}
	return v
}

func envInt(key string, fallback int, fail func(string, ...any)) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		fail("%s must be an integer, got %q", key, raw)
		return fallback
	}
	return v
}

func envFloat(key string, fallback float64, fail func(string, ...any)) float64 {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		fail("%s must be a number, got %q", key, raw)
		return fallback
	}
	return v
}

func envBool(key string, fallback bool, fail func(string, ...any)) bool {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		fail("%s must be true or false, got %q", key, raw)
		return fallback
	}
	return v
}

func envDuration(key string, fallback time.Duration, fail func(string, ...any)) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		fail("%s must be a duration such as 60m, got %q", key, raw)
		return fallback
	}
	return v
}
