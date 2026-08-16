package domain

import (
	"testing"
	"time"
)

// The C# suite also asserted that the NCRONTAB expression agreed with the configured window. That
// test does not port: there is no cron here, the control loop derives its own tick times from these
// same options. The equivalent guard lives with the loop.

func testWindow(t *testing.T) *PollingWindow {
	t.Helper()
	w, err := NewPollingWindow(PollingWindowOptions{
		TimeZone:       "America/Chicago",
		StartHourLocal: 9,
		EndHourLocal:   18,
	})
	if err != nil {
		t.Fatalf("building window: %v", err)
	}
	return w
}

func utc(year int, month time.Month, day, hour, min int) time.Time {
	return time.Date(year, month, day, hour, min, 0, 0, time.UTC)
}

func TestPollingWindowOpenAndClosed(t *testing.T) {
	w := testWindow(t)

	cases := []struct {
		name    string
		instant time.Time
		want    bool
	}{
		{"middle of a summer day (14:00 CDT)", utc(2026, 8, 11, 19, 0), true},
		{"first fire of the day (09:00 CDT)", utc(2026, 8, 11, 14, 0), true},
		{"last fire of the day (17:40 CDT)", utc(2026, 8, 11, 22, 40), true},
		{"end boundary is exclusive (18:00 CDT)", utc(2026, 8, 11, 23, 0), false},
		{"before the start boundary (08:59 CDT)", utc(2026, 8, 11, 13, 59), false},
		{"overnight (03:00 CDT)", utc(2026, 8, 11, 8, 0), false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := w.IsOpen(c.instant); got != c.want {
				t.Errorf("IsOpen(%s) = %v, want %v", c.instant, got, c.want)
			}
		})
	}
}

func TestPollingWindowTracksTheDstBoundary(t *testing.T) {
	// In January, Chicago is CST (UTC-6), so 09:00 local is 15:00 UTC — an hour later in UTC than
	// the same local time in summer. A UTC-only check would poll at the wrong hours half the year.
	w := testWindow(t)

	if w.IsOpen(utc(2026, 1, 15, 14, 0)) { // 08:00 CST
		t.Error("expected closed at 08:00 CST")
	}
	if !w.IsOpen(utc(2026, 1, 15, 15, 0)) { // 09:00 CST
		t.Error("expected open at 09:00 CST")
	}
}

func TestPollingWindowResolvesTheIanaZone(t *testing.T) {
	if id := testWindow(t).TimeZoneID(); id == "" {
		t.Error("expected a resolved zone id")
	}
}

func TestPollingWindowRejectsAnUnknownZone(t *testing.T) {
	if _, err := NewPollingWindow(PollingWindowOptions{TimeZone: "Not/AZone"}); err == nil {
		t.Error("expected an error for an unknown zone")
	}
}
