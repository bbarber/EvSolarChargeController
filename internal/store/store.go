// Package store persists everything the controller needs to survive a restart: the latest known
// state per vehicle, a rolling window of solar readings, a monthly outbound-call counter, and the
// rotating OAuth refresh tokens.
//
// SQLite replaces the Azure Table Storage layer this project used to run on. A single file on the
// host is enough — the whole dataset is a handful of rows, and the only writer is this process.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so it cross-compiles to arm64 cleanly

	"github.com/bbarber/EvSolarChargeController/internal/domain"
)

const schema = `
CREATE TABLE IF NOT EXISTS vehicle_state (
    vin                      TEXT PRIMARY KEY,
    charge_amps              INTEGER,
    reported_max_amps        INTEGER,
    battery_level_percent    INTEGER,
    soc_stop_issued_at       TEXT,
    low_solar_stop_issued_at TEXT,
    charging_state           TEXT    NOT NULL,
    override_active          INTEGER NOT NULL,
    override_detected_at     TEXT,
    last_set_amps            INTEGER,
    last_set_at              TEXT,
    last_updated             TEXT    NOT NULL,
    online                   INTEGER,
    online_at                TEXT
);

CREATE TABLE IF NOT EXISTS solar_readings (
    reading_at TEXT PRIMARY KEY,
    watts      REAL NOT NULL,
    amps       REAL NOT NULL
);

CREATE TABLE IF NOT EXISTS api_usage (
    provider      TEXT    NOT NULL,
    month         TEXT    NOT NULL,
    call_count    INTEGER NOT NULL DEFAULT 0,
    failure_count INTEGER NOT NULL DEFAULT 0,
    last_call_at  TEXT,
    PRIMARY KEY (provider, month)
);

CREATE TABLE IF NOT EXISTS secrets (
    name       TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
`

type Store struct {
	db *sql.DB
}

// Open creates or opens the database at path and applies the schema.
//
// The file holds live OAuth refresh tokens, so it is forced to 0600 on every open rather than
// trusting whatever umask created it.
func Open(path string) (*Store, error) {
	// WAL survives an unclean shutdown without corrupting the file, which matters on a box that
	// may lose power. busy_timeout covers the brief overlap between the telemetry writer and the
	// control loop.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}

	// Columns added after the first deployment. CREATE TABLE IF NOT EXISTS does nothing to a table
	// that already exists, so existing databases need these explicitly; a duplicate-column error
	// just means this has already run.
	for _, stmt := range []string{
		`ALTER TABLE vehicle_state ADD COLUMN online INTEGER`,
		`ALTER TABLE vehicle_state ADD COLUMN online_at TEXT`,
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("migrating: %w", err)
		}
	}

	if path != ":memory:" {
		if err := os.Chmod(path, 0o600); err != nil && !os.IsNotExist(err) {
			db.Close()
			return nil, fmt.Errorf("securing %s: %w", path, err)
		}
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// ---------------------------------------------------------------------------
// Time helpers. Everything is stored as RFC3339 in UTC so string ordering and
// chronological ordering agree, which is what makes the solar range query work.
// ---------------------------------------------------------------------------

const timeLayout = time.RFC3339Nano

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

func nullInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func parseTime(v sql.NullString) (*time.Time, error) {
	if !v.Valid {
		return nil, nil
	}
	parsed, err := time.Parse(timeLayout, v.String)
	if err != nil {
		return nil, fmt.Errorf("parsing time %q: %w", v.String, err)
	}
	return &parsed, nil
}

func intFrom(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	i := int(v.Int64)
	return &i
}

// ---------------------------------------------------------------------------
// Vehicle state
// ---------------------------------------------------------------------------

const vehicleColumns = `vin, charge_amps, reported_max_amps, battery_level_percent,
    soc_stop_issued_at, low_solar_stop_issued_at, charging_state, override_active,
    override_detected_at, last_set_amps, last_set_at, last_updated, online, online_at`

// GetVehicleState returns nil (with no error) when the VIN has never reported.
func (s *Store) GetVehicleState(ctx context.Context, vin string) (*domain.VehicleState, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+vehicleColumns+` FROM vehicle_state WHERE vin = ?`, vin)

	state, err := scanVehicleState(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return state, err
}

// LatestVehicleState returns whichever managed vehicle reported most recently.
//
// One wall connector is shared between the cars, so at most one is plugged in at a time; the most
// recent reporter is the one the control loop should act on.
func (s *Store) LatestVehicleState(ctx context.Context) (*domain.VehicleState, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+vehicleColumns+` FROM vehicle_state ORDER BY last_updated DESC LIMIT 1`)

	state, err := scanVehicleState(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return state, err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanVehicleState(row scanner) (*domain.VehicleState, error) {
	var (
		v                domain.VehicleState
		chargeAmps       sql.NullInt64
		reportedMax      sql.NullInt64
		batteryLevel     sql.NullInt64
		socStop          sql.NullString
		lowSolarStop     sql.NullString
		chargingState    string
		overrideActive   int
		overrideDetected sql.NullString
		lastSetAmps      sql.NullInt64
		lastSetAt        sql.NullString
		lastUpdated      string
		online           sql.NullInt64
		onlineAt         sql.NullString
	)

	if err := row.Scan(&v.VIN, &chargeAmps, &reportedMax, &batteryLevel, &socStop, &lowSolarStop,
		&chargingState, &overrideActive, &overrideDetected, &lastSetAmps, &lastSetAt, &lastUpdated,
		&online, &onlineAt); err != nil {
		return nil, err
	}

	if online.Valid {
		b := online.Int64 != 0
		v.Online = &b
	}

	v.ChargeAmps = intFrom(chargeAmps)
	v.ReportedMaxAmps = intFrom(reportedMax)
	v.BatteryLevelPercent = intFrom(batteryLevel)
	v.ChargingState = domain.ParseChargingState(chargingState)
	v.OverrideActive = overrideActive != 0
	v.LastSetAmps = intFrom(lastSetAmps)

	var err error
	if v.SocStopIssuedAt, err = parseTime(socStop); err != nil {
		return nil, err
	}
	if v.LowSolarStopIssuedAt, err = parseTime(lowSolarStop); err != nil {
		return nil, err
	}
	if v.OverrideDetectedAt, err = parseTime(overrideDetected); err != nil {
		return nil, err
	}
	if v.LastSetAt, err = parseTime(lastSetAt); err != nil {
		return nil, err
	}
	if v.OnlineAt, err = parseTime(onlineAt); err != nil {
		return nil, err
	}
	if v.LastUpdated, err = time.Parse(timeLayout, lastUpdated); err != nil {
		return nil, fmt.Errorf("parsing last_updated %q: %w", lastUpdated, err)
	}

	return &v, nil
}

func (s *Store) SaveVehicleState(ctx context.Context, v *domain.VehicleState) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO vehicle_state (`+vehicleColumns+`)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(vin) DO UPDATE SET
            charge_amps              = excluded.charge_amps,
            reported_max_amps        = excluded.reported_max_amps,
            battery_level_percent    = excluded.battery_level_percent,
            soc_stop_issued_at       = excluded.soc_stop_issued_at,
            low_solar_stop_issued_at = excluded.low_solar_stop_issued_at,
            charging_state           = excluded.charging_state,
            override_active          = excluded.override_active,
            override_detected_at     = excluded.override_detected_at,
            last_set_amps            = excluded.last_set_amps,
            last_set_at              = excluded.last_set_at,
            last_updated             = excluded.last_updated,
            online                   = excluded.online,
            online_at                = excluded.online_at`,
		v.VIN, nullInt(v.ChargeAmps), nullInt(v.ReportedMaxAmps), nullInt(v.BatteryLevelPercent),
		nullTime(v.SocStopIssuedAt), nullTime(v.LowSolarStopIssuedAt), v.ChargingState.String(),
		boolToInt(v.OverrideActive), nullTime(v.OverrideDetectedAt), nullInt(v.LastSetAmps),
		nullTime(v.LastSetAt), formatTime(v.LastUpdated), nullBool(v.Online), nullTime(v.OnlineAt))
	if err != nil {
		return fmt.Errorf("saving vehicle state for %s: %w", v.VIN, err)
	}
	return nil
}

func nullBool(v *bool) any {
	if v == nil {
		return nil
	}
	return boolToInt(*v)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// Solar readings
// ---------------------------------------------------------------------------

func (s *Store) AddSolarReading(ctx context.Context, at time.Time, watts, amps float64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO solar_readings (reading_at, watts, amps) VALUES (?, ?, ?)
         ON CONFLICT(reading_at) DO UPDATE SET watts = excluded.watts, amps = excluded.amps`,
		formatTime(at), watts, amps)
	if err != nil {
		return fmt.Errorf("adding solar reading: %w", err)
	}
	return nil
}

// MaxAmpsSince returns the largest amp-equivalent recorded at or after since, or nil when the
// window holds no readings.
//
// Nil is meaningfully different from zero: a failed Enphase poll leaves the window empty, and that
// is not evidence of low production, so it must not stop a running charge session.
func (s *Store) MaxAmpsSince(ctx context.Context, since time.Time) (*float64, error) {
	var max sql.NullFloat64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(amps) FROM solar_readings WHERE reading_at >= ?`, formatTime(since)).Scan(&max)
	if err != nil {
		return nil, fmt.Errorf("reading solar maximum: %w", err)
	}
	if !max.Valid {
		return nil, nil
	}
	return &max.Float64, nil
}

// PruneSolarReadings drops readings older than before, keeping the table bounded.
func (s *Store) PruneSolarReadings(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM solar_readings WHERE reading_at < ?`, formatTime(before))
	if err != nil {
		return 0, fmt.Errorf("pruning solar readings: %w", err)
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------------------
// API usage
// ---------------------------------------------------------------------------

func monthKey(at time.Time) string { return at.UTC().Format("2006-01") }

// RecordAPICall increments the month's counter and returns the new total, so the caller can
// compare it against the plan's budget before spending the next call.
func (s *Store) RecordAPICall(ctx context.Context, provider string, at time.Time, failed bool) (int, error) {
	failures := 0
	if failed {
		failures = 1
	}

	_, err := s.db.ExecContext(ctx, `
        INSERT INTO api_usage (provider, month, call_count, failure_count, last_call_at)
        VALUES (?, ?, 1, ?, ?)
        ON CONFLICT(provider, month) DO UPDATE SET
            call_count    = call_count + 1,
            failure_count = failure_count + excluded.failure_count,
            last_call_at  = excluded.last_call_at`,
		provider, monthKey(at), failures, formatTime(at))
	if err != nil {
		return 0, fmt.Errorf("recording api call for %s: %w", provider, err)
	}

	return s.MonthlyCallCount(ctx, provider, at)
}

func (s *Store) MonthlyCallCount(ctx context.Context, provider string, at time.Time) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(call_count, 0) FROM api_usage WHERE provider = ? AND month = ?`,
		provider, monthKey(at)).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading api usage for %s: %w", provider, err)
	}
	return count, nil
}

// ---------------------------------------------------------------------------
// Secrets
// ---------------------------------------------------------------------------

// ErrSecretNotFound is returned when a named secret has never been stored.
var ErrSecretNotFound = errors.New("secret not found")

// GetSecret reads a stored value. Refresh tokens rotate on every use, so the stored copy — not the
// seeded environment value — is authoritative once the first refresh has happened.
func (s *Store) GetSecret(ctx context.Context, name string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM secrets WHERE name = ?`, name).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrSecretNotFound, name)
	}
	if err != nil {
		return "", fmt.Errorf("reading secret %s: %w", name, err)
	}
	return value, nil
}

func (s *Store) PutSecret(ctx context.Context, name, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO secrets (name, value, updated_at) VALUES (?, ?, ?)
         ON CONFLICT(name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		name, value, formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("writing secret %s: %w", name, err)
	}
	return nil
}
