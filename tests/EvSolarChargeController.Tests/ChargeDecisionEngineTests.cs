using EvSolarChargeController.Functions.Configuration;
using EvSolarChargeController.Functions.Domain;
using EvSolarChargeController.Functions.Storage;
using FluentAssertions;
using Xunit;

namespace EvSolarChargeController.Tests;

public class ChargeDecisionEngineTests
{
    private static readonly DateTimeOffset Now = new(2026, 8, 11, 14, 0, 0, TimeSpan.Zero);

    private static ChargingOptions Options() => new()
    {
        SystemVoltage = 240,
        MinChargeAmps = 5,
        MaxChargeAmps = 16,
    };

    private static VehicleStateEntity Vehicle(
        ChargingState state = ChargingState.Charging,
        bool overrideActive = false,
        int? lastSetAmps = null,
        int? reportedMax = null,
        DateTimeOffset? lastUpdated = null) => new()
        {
            PartitionKey = "5YJ3E1EA7KF000001",
            RowKey = VehicleStateEntity.StateRowKey,
            ChargingState = state.ToString(),
            OverrideActive = overrideActive,
            LastSetAmps = lastSetAmps,
            ReportedMaxAmps = reportedMax,
            LastUpdated = lastUpdated ?? Now.AddMinutes(-2),
        };

    [Fact]
    public void Sets_amps_from_the_trailing_window_maximum()
    {
        var decision = ChargeDecisionEngine.Decide(Vehicle(), maxAmpsLastHour: 12.3, Options(), Now);

        decision.Action.Should().Be(ChargeAction.SetAmps);
        decision.TargetAmps.Should().Be(12);
        decision.ShouldSend.Should().BeTrue();
    }

    [Fact]
    public void Skips_when_no_telemetry_has_arrived()
    {
        var decision = ChargeDecisionEngine.Decide(null, 12, Options(), Now);

        decision.Action.Should().Be(ChargeAction.SkipNoVehicleState);
        decision.ShouldSend.Should().BeFalse();
    }

    [Fact]
    public void Skips_when_telemetry_is_stale_because_the_car_is_probably_asleep()
    {
        var stale = Vehicle(lastUpdated: Now.AddHours(-9));

        var decision = ChargeDecisionEngine.Decide(stale, 12, Options(), Now);

        decision.Action.Should().Be(ChargeAction.SkipNoVehicleState);
    }

    [Theory]
    [InlineData(ChargingState.Disconnected)]
    [InlineData(ChargingState.Complete)]
    [InlineData(ChargingState.Stopped)]
    [InlineData(ChargingState.NoPower)]
    [InlineData(ChargingState.Unknown)]
    public void Never_commands_a_vehicle_that_is_not_actively_charging(ChargingState state)
    {
        var decision = ChargeDecisionEngine.Decide(Vehicle(state), 12, Options(), Now);

        decision.Action.Should().Be(ChargeAction.SkipNotCharging);
        decision.ShouldSend.Should().BeFalse();
    }

    [Fact]
    public void Respects_an_active_manual_override()
    {
        var decision = ChargeDecisionEngine.Decide(Vehicle(overrideActive: true), 12, Options(), Now);

        decision.Action.Should().Be(ChargeAction.SkipOverrideActive);
    }

    [Fact]
    public void Override_takes_precedence_over_everything_except_staleness()
    {
        var vehicle = Vehicle(overrideActive: true, lastSetAmps: 5);

        var decision = ChargeDecisionEngine.Decide(vehicle, 16, Options(), Now);

        decision.Action.Should().Be(ChargeAction.SkipOverrideActive);
    }

    [Fact]
    public void Skips_when_the_window_holds_no_readings()
    {
        var decision = ChargeDecisionEngine.Decide(Vehicle(), maxAmpsLastHour: null, Options(), Now);

        decision.Action.Should().Be(ChargeAction.SkipNoSolarData);
    }

    [Fact]
    public void Skips_a_redundant_command_when_already_at_target()
    {
        var decision = ChargeDecisionEngine.Decide(Vehicle(lastSetAmps: 12), 12.1, Options(), Now);

        decision.Action.Should().Be(ChargeAction.SkipAlreadyAtTarget);
    }

    [Fact]
    public void Stops_instead_of_clamping_up_when_production_cannot_cover_the_minimum()
    {
        // Clamping 0.7A up to the 5A minimum would pull the remaining ~1kW from the grid.
        // See LowSolarTests for the full stop/resume behaviour.
        var decision = ChargeDecisionEngine.Decide(Vehicle(), maxAmpsLastHour: 0.7, Options(), Now);

        decision.Action.Should().Be(ChargeAction.StopChargingLowSolar);
        decision.TargetAmps.Should().BeNull();
    }

    [Fact]
    public void Clamps_down_to_the_configured_ceiling()
    {
        var decision = ChargeDecisionEngine.Decide(Vehicle(), maxAmpsLastHour: 48, Options(), Now);

        decision.TargetAmps.Should().Be(16);
    }

    [Fact]
    public void Honours_a_lower_vehicle_reported_maximum()
    {
        // Asking for more than the car will accept would make every cycle look like a mismatch
        // and falsely trip override detection.
        var decision = ChargeDecisionEngine.Decide(Vehicle(reportedMax: 12), maxAmpsLastHour: 16, Options(), Now);

        decision.TargetAmps.Should().Be(12);
    }

    [Fact]
    public void Ignores_an_implausible_vehicle_reported_maximum()
    {
        var decision = ChargeDecisionEngine.Decide(Vehicle(reportedMax: 0), maxAmpsLastHour: 16, Options(), Now);

        decision.TargetAmps.Should().Be(16);
    }
}
