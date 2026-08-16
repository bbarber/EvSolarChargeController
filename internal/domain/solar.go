package domain

import (
	"errors"
	"fmt"
	"math"
)

var ErrNonPositiveVoltage = errors.New("system voltage must be positive")

// WattsToAmps converts instantaneous production in watts to the equivalent charge current at the
// configured service voltage. Net metering can report negative production, which is treated as
// none rather than as a negative current.
func WattsToAmps(watts, systemVoltage float64) (float64, error) {
	if systemVoltage <= 0 {
		return 0, fmt.Errorf("%w: %v", ErrNonPositiveVoltage, systemVoltage)
	}
	if watts <= 0 {
		return 0, nil
	}
	return watts / systemVoltage, nil
}

// ToRequestableAmps rounds a fractional amp figure to the integer the Fleet API accepts and clamps
// it into the acceptable range. math.Round rounds half away from zero, which — combined with taking
// the maximum over the trailing window — biases toward overshoot.
//
// Note this clamps *up* to minAmps. Callers must decide separately whether production can actually
// sustain the minimum; see ChargeDecisionEngine, which stops the session rather than clamping up
// into a grid draw.
func ToRequestableAmps(amps float64, minAmps, maxAmps int) (int, error) {
	if minAmps > maxAmps {
		return 0, fmt.Errorf("MinChargeAmps (%d) cannot exceed MaxChargeAmps (%d)", minAmps, maxAmps)
	}

	rounded := int(math.Round(amps))
	if rounded < minAmps {
		return minAmps, nil
	}
	if rounded > maxAmps {
		return maxAmps, nil
	}
	return rounded, nil
}
