package domain

import (
	"errors"
	"math"
	"testing"
)

func TestWattsToAmpsConvertsAtTheConfiguredVoltage(t *testing.T) {
	cases := []struct {
		watts, voltage, want float64
	}{
		{3840, 240, 16},
		{1200, 240, 5},
		{0, 240, 0},
		{-50, 240, 0}, // Net metering can report negative; treat as no production.
	}

	for _, c := range cases {
		got, err := WattsToAmps(c.watts, c.voltage)
		if err != nil {
			t.Fatalf("WattsToAmps(%v, %v) returned error: %v", c.watts, c.voltage, err)
		}
		if math.Abs(got-c.want) > 0.001 {
			t.Errorf("WattsToAmps(%v, %v) = %v, want %v", c.watts, c.voltage, got, c.want)
		}
	}
}

func TestWattsToAmpsRejectsANonPositiveVoltage(t *testing.T) {
	if _, err := WattsToAmps(1000, 0); !errors.Is(err, ErrNonPositiveVoltage) {
		t.Errorf("expected ErrNonPositiveVoltage, got %v", err)
	}
}

func TestToRequestableAmpsRoundsAwayFromZero(t *testing.T) {
	cases := []struct {
		amps float64
		want int
	}{
		{11.4, 11},
		{11.5, 12}, // Rounds away from zero, biasing toward overshoot.
		{15.9, 16},
	}

	for _, c := range cases {
		got, err := ToRequestableAmps(c.amps, 5, 16)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != c.want {
			t.Errorf("ToRequestableAmps(%v) = %d, want %d", c.amps, got, c.want)
		}
	}
}

func TestToRequestableAmpsClampsIntoRange(t *testing.T) {
	cases := []struct {
		amps float64
		want int
	}{
		{0.4, 5}, // Below the connector minimum, clamp up.
		{2, 5},
		{40, 16}, // Above the configured ceiling, clamp down.
		{16.4, 16},
	}

	for _, c := range cases {
		got, err := ToRequestableAmps(c.amps, 5, 16)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != c.want {
			t.Errorf("ToRequestableAmps(%v) = %d, want %d", c.amps, got, c.want)
		}
	}
}

func TestToRequestableAmpsRejectsAnInvertedRange(t *testing.T) {
	if _, err := ToRequestableAmps(10, 20, 16); err == nil {
		t.Error("expected an error for min > max")
	}
}
