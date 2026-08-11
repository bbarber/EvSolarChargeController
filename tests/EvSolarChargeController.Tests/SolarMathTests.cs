using EvSolarChargeController.Functions.Domain;
using FluentAssertions;
using Xunit;

namespace EvSolarChargeController.Tests;

public class SolarMathTests
{
    [Theory]
    [InlineData(3840, 240, 16)]
    [InlineData(1200, 240, 5)]
    [InlineData(0, 240, 0)]
    [InlineData(-50, 240, 0)] // Net-metering can report negative; treat as no production.
    public void WattsToAmps_converts_at_the_configured_voltage(double watts, double voltage, double expected)
    {
        SolarMath.WattsToAmps(watts, voltage).Should().BeApproximately(expected, 0.001);
    }

    [Fact]
    public void WattsToAmps_rejects_a_non_positive_voltage()
    {
        var act = () => SolarMath.WattsToAmps(1000, 0);
        act.Should().Throw<ArgumentOutOfRangeException>();
    }

    [Theory]
    [InlineData(11.4, 11)]
    [InlineData(11.5, 12)] // Rounds away from zero, biasing toward overshoot.
    [InlineData(15.9, 16)]
    public void ToRequestableAmps_rounds_away_from_zero(double amps, int expected)
    {
        SolarMath.ToRequestableAmps(amps, 5, 16).Should().Be(expected);
    }

    [Theory]
    [InlineData(0.4, 5)]   // Below the connector minimum, clamp up.
    [InlineData(2, 5)]
    [InlineData(40, 16)]   // Above the configured ceiling, clamp down.
    [InlineData(16.4, 16)]
    public void ToRequestableAmps_clamps_into_range(double amps, int expected)
    {
        SolarMath.ToRequestableAmps(amps, 5, 16).Should().Be(expected);
    }

    [Fact]
    public void ToRequestableAmps_rejects_an_inverted_range()
    {
        var act = () => SolarMath.ToRequestableAmps(10, minAmps: 20, maxAmps: 16);
        act.Should().Throw<ArgumentException>();
    }
}
