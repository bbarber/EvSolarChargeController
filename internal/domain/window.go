package domain

import (
	"fmt"
	"time"
)

// PollingWindow decides whether a given instant falls inside the daylight polling window.
//
// The scheduler already restricts firing to daytime hours, but the window is re-checked here
// against an explicitly resolved zone so a misconfigured schedule cannot quietly spend the Enphase
// monthly budget on overnight calls.
type PollingWindow struct {
	opts     PollingWindowOptions
	location *time.Location
}

func NewPollingWindow(opts PollingWindowOptions) (*PollingWindow, error) {
	loc, err := time.LoadLocation(opts.TimeZone)
	if err != nil {
		return nil, fmt.Errorf("resolving time zone %q: %w", opts.TimeZone, err)
	}
	return &PollingWindow{opts: opts, location: loc}, nil
}

func (w *PollingWindow) TimeZoneID() string { return w.location.String() }

func (w *PollingWindow) ToLocal(instant time.Time) time.Time { return instant.In(w.location) }

// IsOpen reports whether instant falls within [StartHourLocal, EndHourLocal) local time.
//
// Resolving against a named zone rather than a fixed offset is what makes this correct across the
// daylight-saving boundary: 09:00 local is 14:00 UTC in summer and 15:00 UTC in winter.
func (w *PollingWindow) IsOpen(instant time.Time) bool {
	hour := w.ToLocal(instant).Hour()
	return hour >= w.opts.StartHourLocal && hour < w.opts.EndHourLocal
}

func (w *PollingWindow) Describe(instant time.Time) string {
	local := w.ToLocal(instant)
	return fmt.Sprintf("%s %s (window %02d:00-%02d:00)",
		local.Format("2006-01-02 15:04"),
		w.location.String(),
		w.opts.StartHourLocal,
		w.opts.EndHourLocal)
}
