// Package pause reads the dashboard's "stop touching my car" switch.
//
// The switch lives in Supabase because that is where the dashboard can write it. The VM *polls*
// for it and never listens: no port is opened, no webhook is exposed, nothing inbound is added to
// a box whose only job is to hold a signing key. An attacker who takes the dashboard can pause
// charging, which is the safe direction — they cannot start a session, raise a current, or reach
// the VM.
package pause

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// DefaultPollInterval is how often the switch is re-read. Frequent enough that pressing the button
// takes effect before the next decision, cheap enough to ignore: the response is a few dozen bytes.
const DefaultPollInterval = 30 * time.Second

// Switch is the cached state of the dashboard's pause control.
//
// The zero value, and a nil *Switch, both report "not paused" — so a deployment with no Supabase
// configured behaves exactly as it did before this existed.
type Switch struct {
	url      string
	key      string
	interval time.Duration
	http     *http.Client
	log      *slog.Logger

	mu     sync.RWMutex
	until  *time.Time
	loaded bool
}

func New(supabaseURL, serviceKey string, log *slog.Logger) *Switch {
	if supabaseURL == "" || serviceKey == "" {
		return nil
	}
	return &Switch{
		url:      supabaseURL,
		key:      serviceKey,
		interval: DefaultPollInterval,
		http:     &http.Client{Timeout: 10 * time.Second},
		log:      log,
	}
}

// Paused reports whether commands to the vehicle are currently withheld.
//
// The stored value is an instant, not a boolean, so the pause expires on its own at whatever
// midnight the dashboard picked. Nothing has to run at midnight to clear it, and a VM that was
// offline over the boundary comes back un-paused rather than stuck.
func (s *Switch) Paused(now time.Time) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.until != nil && now.Before(*s.until)
}

// Until is when the current pause lapses, or nil when not paused. For logging.
func (s *Switch) Until() *time.Time {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.until == nil {
		return nil
	}
	t := *s.until
	return &t
}

// Run polls until the context is cancelled. Fetch failures are logged and otherwise ignored: the
// last known state stands.
//
// That direction is deliberate. Charging must never depend on Supabase being reachable, so an
// outage cannot pause the system; and equally, an outage cannot silently *un*-pause it, because
// the cached instant is kept rather than cleared.
func (s *Switch) Run(ctx context.Context) {
	if s == nil {
		return
	}

	s.refresh(ctx)

	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.refresh(ctx)
		}
	}
}

func (s *Switch) refresh(ctx context.Context) {
	until, err := s.fetch(ctx)
	if err != nil {
		s.log.Warn("reading the pause switch", "error", err)
		return
	}

	s.mu.Lock()
	changed := s.loaded && !sameInstant(s.until, until)
	first := !s.loaded
	s.until, s.loaded = until, true
	s.mu.Unlock()

	if changed || first {
		if until == nil {
			s.log.Info("pause switch is off; commands allowed")
		} else {
			s.log.Info("pause switch is on; withholding commands", "until", until.UTC())
		}
	}
}

func (s *Switch) fetch(ctx context.Context) (*time.Time, error) {
	endpoint := fmt.Sprintf("%s/rest/v1/system_control?select=paused_until&limit=1",
		s.url)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("apikey", s.key)
	req.Header.Set("Authorization", "Bearer "+s.key)

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("pause switch returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var rows []struct {
		PausedUntil *string `json:"paused_until"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("unreadable pause switch response: %w", err)
	}
	// No row and a null column mean the same thing: nobody has paused anything.
	if len(rows) == 0 || rows[0].PausedUntil == nil {
		return nil, nil
	}

	parsed, err := time.Parse(time.RFC3339, *rows[0].PausedUntil)
	if err != nil {
		return nil, fmt.Errorf("unparseable paused_until %q: %w", *rows[0].PausedUntil, err)
	}
	utc := parsed.UTC()
	return &utc, nil
}

func sameInstant(a, b *time.Time) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.Equal(*b)
	}
}

// endpointHost is used only in tests to assert nothing but Supabase is contacted.
func (s *Switch) endpointHost() string {
	u, err := url.Parse(s.url)
	if err != nil {
		return ""
	}
	return u.Host
}
