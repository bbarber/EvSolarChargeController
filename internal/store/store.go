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
    charging_state           TEXT    NOT NULL,
    session                  TEXT    NOT NULL DEFAULT 'Auto',
    session_since            TEXT,
    last_set_amps            INTEGER,
    last_set_at              TEXT,
    last_updated             TEXT    NOT NULL,
    online                   INTEGER,
    online_at                TEXT,
    at_home                  INTEGER,
    at_home_at               TEXT,
    fast_charger             INTEGER
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

CREATE TABLE IF NOT EXISTS wake_events (
    vin TEXT NOT NULL,
    at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS mirror_outbox (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    tbl          TEXT NOT NULL,
    conflict_key TEXT NOT NULL DEFAULT '',
    payload      BLOB NOT NULL,
    created_at   TEXT NOT NULL
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
		`ALTER TABLE vehicle_state ADD COLUMN last_wake_at TEXT`,
		`ALTER TABLE vehicle_state ADD COLUMN session TEXT NOT NULL DEFAULT 'Auto'`,
		`ALTER TABLE vehicle_state ADD COLUMN session_since TEXT`,
		`ALTER TABLE vehicle_state ADD COLUMN at_home INTEGER`,
		`ALTER TABLE vehicle_state ADD COLUMN at_home_at TEXT`,
		`ALTER TABLE vehicle_state ADD COLUMN fast_charger INTEGER`,
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("migrating: %w", err)
		}
	}

	// Databases written before the session state machine carry its meaning in three marker
	// columns. Fold them into the session once, then DROP the legacy columns — leaving them was
	// not harmless: override_active carried NOT NULL, and an INSERT that no longer supplies it
	// fails on every save. Test databases never had the legacy columns, which is exactly why the
	// suite stayed green while production could not persist a frame.
	if _, err := db.Exec(`
        UPDATE vehicle_state SET
            session = CASE
                WHEN COALESCE(override_active, 0) != 0        THEN 'Overridden'
                WHEN soc_stop_issued_at IS NOT NULL           THEN 'StoppedAtCap'
                WHEN low_solar_stop_issued_at IS NOT NULL     THEN 'StoppedForSun'
                ELSE session END,
            session_since = CASE
                WHEN COALESCE(override_active, 0) != 0        THEN override_detected_at
                WHEN soc_stop_issued_at IS NOT NULL           THEN soc_stop_issued_at
                WHEN low_solar_stop_issued_at IS NOT NULL     THEN low_solar_stop_issued_at
                ELSE session_since END
        WHERE session = 'Auto' AND session_since IS NULL`); err != nil {
		// Fresh databases have no marker columns at all; that is not an error.
		if !strings.Contains(err.Error(), "no such column") {
			db.Close()
			return nil, fmt.Errorf("migrating markers to session: %w", err)
		}
	}

	for _, col := range []string{
		"override_active", "override_detected_at", "soc_stop_issued_at", "low_solar_stop_issued_at",
	} {
		if _, err := db.Exec(`ALTER TABLE vehicle_state DROP COLUMN ` + col); err != nil &&
			!strings.Contains(err.Error(), "no such column") {
			db.Close()
			return nil, fmt.Errorf("dropping legacy column %s: %w", col, err)
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
    charging_state, session, session_since, last_set_amps, last_set_at, last_updated,
    online, online_at, last_wake_at, at_home, at_home_at, fast_charger`

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

type scanner interface {
	Scan(dest ...any) error
}

func scanVehicleState(row scanner) (*domain.VehicleState, error) {
	var (
		v             domain.VehicleState
		chargeAmps    sql.NullInt64
		reportedMax   sql.NullInt64
		batteryLevel  sql.NullInt64
		chargingState string
		session       string
		sessionSince  sql.NullString
		lastSetAmps   sql.NullInt64
		lastSetAt     sql.NullString
		lastUpdated   string
		online        sql.NullInt64
		onlineAt      sql.NullString
		lastWakeAt    sql.NullString
		atHome        sql.NullInt64
		atHomeAt      sql.NullString
		fastCharger   sql.NullInt64
	)

	if err := row.Scan(&v.VIN, &chargeAmps, &reportedMax, &batteryLevel,
		&chargingState, &session, &sessionSince, &lastSetAmps, &lastSetAt, &lastUpdated,
		&online, &onlineAt, &lastWakeAt, &atHome, &atHomeAt, &fastCharger); err != nil {
		return nil, err
	}

	if online.Valid {
		b := online.Int64 != 0
		v.Online = &b
	}
	if atHome.Valid {
		b := atHome.Int64 != 0
		v.AtHome = &b
	}
	if fastCharger.Valid {
		b := fastCharger.Int64 != 0
		v.FastCharger = &b
	}

	v.ChargeAmps = intFrom(chargeAmps)
	v.ReportedMaxAmps = intFrom(reportedMax)
	v.BatteryLevelPercent = intFrom(batteryLevel)
	v.ChargingState = domain.ParseChargingState(chargingState)
	v.Session = domain.ParseSessionState(session)
	v.LastSetAmps = intFrom(lastSetAmps)

	var err error
	if v.SessionSince, err = parseTime(sessionSince); err != nil {
		return nil, err
	}
	if v.LastSetAt, err = parseTime(lastSetAt); err != nil {
		return nil, err
	}
	if v.OnlineAt, err = parseTime(onlineAt); err != nil {
		return nil, err
	}
	if v.LastWakeAt, err = parseTime(lastWakeAt); err != nil {
		return nil, err
	}
	if v.AtHomeAt, err = parseTime(atHomeAt); err != nil {
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
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(vin) DO UPDATE SET
            charge_amps           = excluded.charge_amps,
            reported_max_amps     = excluded.reported_max_amps,
            battery_level_percent = excluded.battery_level_percent,
            charging_state        = excluded.charging_state,
            session               = excluded.session,
            session_since         = excluded.session_since,
            last_set_amps         = excluded.last_set_amps,
            last_set_at           = excluded.last_set_at,
            last_updated          = excluded.last_updated,
            online                = excluded.online,
            online_at             = excluded.online_at,
            last_wake_at          = excluded.last_wake_at,
            at_home               = excluded.at_home,
            at_home_at            = excluded.at_home_at,
            fast_charger          = excluded.fast_charger`,
		v.VIN, nullInt(v.ChargeAmps), nullInt(v.ReportedMaxAmps), nullInt(v.BatteryLevelPercent),
		v.ChargingState.String(), v.Session.String(), nullTime(v.SessionSince),
		nullInt(v.LastSetAmps), nullTime(v.LastSetAt), formatTime(v.LastUpdated),
		nullBool(v.Online), nullTime(v.OnlineAt), nullTime(v.LastWakeAt),
		nullBool(v.AtHome), nullTime(v.AtHomeAt), nullBool(v.FastCharger))
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

// SolarReading is one production sample, for the mirror's backfill.
type SolarReading struct {
	At    time.Time
	Watts float64
	Amps  float64
}

// AllSolarReadings returns every retained reading, oldest first.
func (s *Store) AllSolarReadings(ctx context.Context) ([]SolarReading, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT reading_at, watts, amps FROM solar_readings ORDER BY reading_at`)
	if err != nil {
		return nil, fmt.Errorf("reading all solar readings: %w", err)
	}
	defer rows.Close()

	var out []SolarReading
	for rows.Next() {
		var at string
		var r SolarReading
		if err := rows.Scan(&at, &r.Watts, &r.Amps); err != nil {
			return nil, err
		}
		if r.At, err = time.Parse(timeLayout, at); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReadingsAboveSince counts readings at or after since that clear minAmps. More than one is what
// distinguishes a sustained window from a single sunbreak.
func (s *Store) ReadingsAboveSince(ctx context.Context, since time.Time, minAmps float64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM solar_readings WHERE reading_at >= ? AND amps >= ?`,
		formatTime(since), minAmps).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting solar readings: %w", err)
	}
	return n, nil
}

// PruneSolarReadings drops readings older than before.
//
// Nothing calls this. Readings are kept forever: a year is roughly a megabyte, and the accumulated
// history is the only real measurement of this array in existence. Retained as a manual operation
// in case the table ever does need trimming — but note that sharing a prune horizon with a query
// window is exactly how the wake gate was silently broken, so any caller should own its own.
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
// Mirror outbox
// ---------------------------------------------------------------------------

func (s *Store) EnqueueMirror(ctx context.Context, table, conflictKey string, payload []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO mirror_outbox (tbl, conflict_key, payload, created_at) VALUES (?, ?, ?, ?)`,
		table, conflictKey, payload, formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("enqueueing mirror row for %s: %w", table, err)
	}
	return nil
}

// OutboxRow is one queued mirror shipment.
type OutboxRow struct {
	ID          int64
	Table       string
	ConflictKey string
	Payload     []byte
}

func (s *Store) PendingMirror(ctx context.Context, limit int) ([]OutboxRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tbl, conflict_key, payload FROM mirror_outbox ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("reading the mirror outbox: %w", err)
	}
	defer rows.Close()

	var out []OutboxRow
	for rows.Next() {
		var r OutboxRow
		if err := rows.Scan(&r.ID, &r.Table, &r.ConflictKey, &r.Payload); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) DeleteMirror(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM mirror_outbox WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting mirror row %d: %w", id, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Wakes
// ---------------------------------------------------------------------------

// RecordWake logs a wake. Kept as individual events rather than a counter so the daily limit can
// be enforced on a rolling local day and the cost stays auditable.
func (s *Store) RecordWake(ctx context.Context, vin string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO wake_events (vin, at) VALUES (?, ?)`, vin, formatTime(at))
	if err != nil {
		return fmt.Errorf("recording wake for %s: %w", vin, err)
	}
	return nil
}

// WakesSince counts wakes at or after since. The caller passes the local midnight, so the day
// boundary matches the human one rather than UTC's.
func (s *Store) WakesSince(ctx context.Context, vin string, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM wake_events WHERE vin = ? AND at >= ?`, vin, formatTime(since)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting wakes for %s: %w", vin, err)
	}
	return n, nil
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
