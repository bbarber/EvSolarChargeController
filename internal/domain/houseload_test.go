package domain

import "testing"

// The consumption meter reports the magnitude of net grid flow, so recovering the house load is a
// sign flip. These cases are taken from real 15-minute intervals on 2026-08-18/19.

func TestHouseLoadAtNightIsTheReportedFigure(t *testing.T) {
	// No production, so net flow is import and equals the load. This is why the overnight numbers
	// were always plausible while the daytime ones were not.
	if got := HouseLoadWatts(152, 0); got != 152 {
		t.Errorf("HouseLoadWatts(152, 0) = %v, want 152", got)
	}
}

func TestHouseLoadWhileExportingSubtracts(t *testing.T) {
	// Aug 18 13:30: meter said 1776 W while the array made 3372 W. Reported below production means
	// the house was exporting, so the real load is the difference — an idle house, not 1776 W.
	if got := HouseLoadWatts(1776, 3372); got != 1596 {
		t.Errorf("HouseLoadWatts(1776, 3372) = %v, want 1596", got)
	}
}

func TestHouseLoadWhileImportingAdds(t *testing.T) {
	// Aug 18 18:00, car charging: meter said 2492 W against 936 W of production. Reported above
	// production can only be import, so the load is the sum.
	if got := HouseLoadWatts(2492, 936); got != 3428 {
		t.Errorf("HouseLoadWatts(2492, 936) = %v, want 3428", got)
	}
}

// The branch boundary: exactly at production, either reading gives the same answer, so the
// function must not disagree with itself across the seam.
func TestHouseLoadIsContinuousAtTheBoundary(t *testing.T) {
	const prod = 2000
	below := HouseLoadWatts(prod-1, prod)
	at := HouseLoadWatts(prod, prod)
	if at != 2*prod {
		t.Errorf("at the boundary got %v, want %v", at, 2*prod)
	}
	// Just below the seam the house is exporting almost everything: load near zero. Just at it,
	// the house is importing its whole load. The jump is inherent to a lost sign, and it is worth
	// a test so nobody "fixes" the discontinuity without understanding it.
	if below != 1 {
		t.Errorf("just below the boundary got %v, want 1", below)
	}
}

func TestHouseLoadNeverGoesNegative(t *testing.T) {
	// Meter noise: reported a hair under production with both essentially zero.
	if got := HouseLoadWatts(10, 4); got != 14 {
		t.Errorf("HouseLoadWatts(10, 4) = %v, want 14", got)
	}
	if got := HouseLoadWatts(0, 0); got != 0 {
		t.Errorf("HouseLoadWatts(0, 0) = %v, want 0", got)
	}
}
