// Package mirror ships the controller's state to Supabase for the dashboard.
//
// SQLite stays authoritative: everything here goes through an outbox table written locally first,
// and a shipper drains it to Supabase's REST API with retry. A Supabase outage therefore costs
// dashboard freshness and nothing else — charging never depends on this package.
package mirror

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/bbarber/EvSolarChargeController/internal/domain"
	"github.com/bbarber/EvSolarChargeController/internal/store"
)

// Outbox is the persistence the mirror needs, implemented by the store. The row type lives with
// the store so this package depends on it and never the reverse.
type Outbox interface {
	EnqueueMirror(ctx context.Context, table, conflictKey string, payload []byte) error
	// PendingMirror returns up to limit rows, oldest first.
	PendingMirror(ctx context.Context, limit int) ([]store.OutboxRow, error)
	DeleteMirror(ctx context.Context, id int64) error
}

type Config struct {
	// URL is the Supabase project URL, e.g. https://xyz.supabase.co
	URL string
	// ServiceKey is the service-role key. It bypasses RLS, which is the point: the mirror is the
	// only writer, and readers are constrained by the policies instead.
	ServiceKey string
}

func (c Config) Enabled() bool { return c.URL != "" && c.ServiceKey != "" }

// Mirror enqueues locally and ships in the background. All Record* methods are cheap, local and
// never fail the caller: a full disk is the only way they error, and the caller just logs it.
type Mirror struct {
	cfg    Config
	outbox Outbox
	http   *http.Client
	log    *slog.Logger
}

func New(cfg Config, outbox Outbox, log *slog.Logger) *Mirror {
	return &Mirror{cfg: cfg, outbox: outbox, http: &http.Client{Timeout: 20 * time.Second}, log: log}
}

// RecordStatus mirrors the vehicle's current snapshot.
func (m *Mirror) RecordStatus(ctx context.Context, v *domain.VehicleState) {
	if m == nil || v == nil {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"vin":               v.VIN,
		"charging_state":    v.ChargingState.String(),
		"session":           v.Session.String(),
		"session_since":     v.SessionSince,
		"charge_amps":       v.ChargeAmps,
		"reported_max_amps": v.ReportedMaxAmps,
		"battery_level":     v.BatteryLevelPercent,
		"last_set_amps":     v.LastSetAmps,
		"last_set_at":       v.LastSetAt,
		"online":            v.Online,
		"at_home":           v.AtHome,
		"fast_charger":      v.FastCharger,
		"last_updated":      v.LastUpdated,
		"mirrored_at":       time.Now().UTC(),
	})
	if err != nil {
		return
	}
	m.enqueue(ctx, "vehicle_status", "vin", payload)
}

// RecordSolar mirrors one production reading, with the house load when it was measured.
func (m *Mirror) RecordSolar(ctx context.Context, at time.Time, watts, amps float64, houseWatts *float64) {
	if m == nil {
		return
	}
	row := map[string]any{"reading_at": at.UTC(), "watts": watts, "amps": amps}
	if houseWatts != nil {
		row["house_watts"] = *houseWatts
	}
	payload, _ := json.Marshal(row)
	m.enqueue(ctx, "solar_readings", "reading_at", payload)
}

// RecordCharge mirrors one sample of what the car is drawing. Zero-amp samples matter: they are
// what closes a step when charging stops, so the graph falls to the baseline instead of holding
// the last current forever.
func (m *Mirror) RecordCharge(ctx context.Context, at time.Time, vin string, amps int, watts float64) {
	if m == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"vin": vin, "reading_at": at.UTC(), "amps": amps, "watts": watts,
	})
	m.enqueue(ctx, "charge_readings", "vin,reading_at", payload)
}

// RecordEvent mirrors one interesting moment: a decision, a connectivity change, a wake, a
// command, an error.
func (m *Mirror) RecordEvent(ctx context.Context, at time.Time, vin, kind, action, reason string) {
	if m == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"at": at.UTC(), "vin": vin, "kind": kind, "action": action, "reason": reason,
	})
	m.enqueue(ctx, "events", "", payload)
}

func (m *Mirror) enqueue(ctx context.Context, table, conflictKey string, payload []byte) {
	if err := m.outbox.EnqueueMirror(ctx, table, conflictKey, payload); err != nil {
		m.log.Warn("enqueueing a mirror row", "table", table, "error", err)
	}
}

// Run drains the outbox until ctx is cancelled. Rows are deleted only after Supabase accepts
// them, so a crash or outage replays rather than loses. Upserts make replay idempotent for
// keyed tables; events may rarely duplicate on a crash mid-batch, which the dashboard tolerates.
func (m *Mirror) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.drain(ctx); err != nil {
				m.log.Warn("draining the mirror outbox", "error", err)
			}
		}
	}
}

func (m *Mirror) drain(ctx context.Context) error {
	rows, err := m.outbox.PendingMirror(ctx, 50)
	if err != nil {
		return err
	}

	for _, row := range rows {
		if err := m.ship(ctx, row); err != nil {
			// Leave the row for the next pass; later rows for other tables would ship out of
			// order with this one, so stop the batch here and keep ordering simple.
			return fmt.Errorf("shipping outbox row %d to %s: %w", row.ID, row.Table, err)
		}
		if err := m.outbox.DeleteMirror(ctx, row.ID); err != nil {
			return err
		}
	}
	return nil
}

func (m *Mirror) ship(ctx context.Context, row store.OutboxRow) error {
	url := fmt.Sprintf("%s/rest/v1/%s", m.cfg.URL, row.Table)
	if row.ConflictKey != "" {
		url += "?on_conflict=" + row.ConflictKey
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(row.Payload))
	if err != nil {
		return err
	}
	req.Header.Set("apikey", m.cfg.ServiceKey)
	req.Header.Set("Authorization", "Bearer "+m.cfg.ServiceKey)
	req.Header.Set("Content-Type", "application/json")
	prefer := "return=minimal"
	if row.ConflictKey != "" {
		prefer += ",resolution=merge-duplicates"
	}
	req.Header.Set("Prefer", prefer)

	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("supabase returned HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}
