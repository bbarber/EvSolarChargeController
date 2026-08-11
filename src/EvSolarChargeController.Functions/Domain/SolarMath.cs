namespace EvSolarChargeController.Functions.Domain;

/// <summary>Pure conversions between solar production and a requestable charge current.</summary>
public static class SolarMath
{
    /// <summary>
    /// Converts instantaneous production in watts to the equivalent charge current at the
    /// configured service voltage. Assumes single-phase / US split-phase 240V by default.
    /// </summary>
    public static double WattsToAmps(double watts, double systemVoltage)
    {
        if (systemVoltage <= 0)
        {
            throw new ArgumentOutOfRangeException(nameof(systemVoltage), systemVoltage, "System voltage must be positive.");
        }

        return watts <= 0 ? 0d : watts / systemVoltage;
    }

    /// <summary>
    /// Rounds a fractional amp figure to the integer the Fleet API accepts and clamps it into the
    /// vehicle's acceptable range. Rounds away from zero, which — combined with taking the maximum
    /// over the trailing hour — biases toward overshoot as specified.
    /// </summary>
    public static int ToRequestableAmps(double amps, int minAmps, int maxAmps)
    {
        if (minAmps > maxAmps)
        {
            throw new ArgumentException($"MinChargeAmps ({minAmps}) cannot exceed MaxChargeAmps ({maxAmps}).", nameof(minAmps));
        }

        var rounded = (int)Math.Round(amps, MidpointRounding.AwayFromZero);
        return Math.Clamp(rounded, minAmps, maxAmps);
    }
}
