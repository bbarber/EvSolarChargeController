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
		ZMQTopic:     env("EVSOLAR_ZMQ_TOPIC", "evsolar_"),
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
	cfg.Charging.WakeToCharge = envBool("EVSOLAR_WAKE_TO_CHARGE", cfg.Charging.WakeToCharge, fail)
	cfg.Charging.MaxWakesPerDay = envInt("EVSOLAR_MAX_WAKES_PER_DAY", cfg.Charging.MaxWakesPerDay, fail)
	cfg.Charging.WakeCooldown = envDuration("EVSOLAR_WAKE_COOLDOWN", cfg.Charging.WakeCooldown, fail)
	cfg.Charging.WakeSocHeadroom = envInt("EVSOLAR_WAKE_SOC_HEADROOM", cfg.Charging.WakeSocHeadroom, fail)
	cfg.Charging.WakeDays = envWeekdays("EVSOLAR_WAKE_DAYS", cfg.Charging.WakeDays, fail)
	cfg.Charging.SustainedWindow = envDuration("EVSOLAR_SUSTAINED_WINDOW", cfg.Charging.SustainedWindow, fail)
	cfg.Charging.HomeLatitude = envFloat("EVSOLAR_HOME_LAT", cfg.Charging.HomeLatitude, fail)
	cfg.Charging.HomeLongitude = envFloat("EVSOLAR_HOME_LON", cfg.Charging.HomeLongitude, fail)
	cfg.Charging.HomeRadiusM = envFloat("EVSOLAR_HOME_RADIUS_M", cfg.Charging.HomeRadiusM, fail)

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
	// The wake gate needs at least two readings inside the sustained window, and readings arrive
	// one per tick. Catching this here beats discovering it as a gate that silently never fires.
	if cfg.Charging.SustainedWindow <= cfg.Charging.LookbackWindow {
		fail("EVSOLAR_SUSTAINED_WINDOW (%s) must be longer than EVSOLAR_LOOKBACK (%s), or fewer than two readings can fall inside it",
			cfg.Charging.SustainedWindow, cfg.Charging.LookbackWindow)
	}
	if cfg.Window.StartHourLocal >= cfg.Window.EndHourLocal {
		fail("EVSOLAR_WINDOW_START_HOUR (%d) must be before EVSOLAR_WINDOW_END_HOUR (%d)",
			cfg.Window.StartHourLocal, cfg.Window.EndHourLocal)
	}
	if (cfg.Charging.HomeLatitude == 0) != (cfg.Charging.HomeLongitude == 0) {
		fail("EVSOLAR_HOME_LAT and EVSOLAR_HOME_LON must be set together")
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

// envWeekdays parses a comma-separated day list such as "Fri,Sat,Sun". An empty value means every
// day; the zero-length slice is meaningful, so it is distinguished from the variable being unset.
func envWeekdays(key string, fallback []time.Weekday, fail func(string, ...any)) []time.Weekday {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	if strings.TrimSpace(raw) == "" {
		return nil // explicitly unrestricted
	}

	names := map[string]time.Weekday{
		"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
		"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
	}

	var out []time.Weekday
	for _, part := range strings.Split(raw, ",") {
		key3 := strings.ToLower(strings.TrimSpace(part))
		if len(key3) > 3 {
			key3 = key3[:3]
		}
		day, known := names[key3]
		if !known {
			fail("%s contains an unrecognised day %q", key, strings.TrimSpace(part))
			continue
		}
		out = append(out, day)
	}
	return out
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
