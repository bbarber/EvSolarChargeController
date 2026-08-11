using EvSolarChargeController.Functions.Configuration;
using EvSolarChargeController.Functions.Domain;
using EvSolarChargeController.Functions.Storage;
using FluentAssertions;
using Xunit;

namespace EvSolarChargeController.Tests;

/// <summary>
/// This controller must never drive charging past the configured state of charge, even when the
/// vehicle's own charge limit is set higher. A person can still charge past it by hand, which is
/// treated as a manual override.
/// </summary>
public class SocCapTests
{
    private const string Vin = "5YJ3E1EA7KF000001";

    private static readonly DateTimeOffset Now = new(2026, 8, 11, 14, 0, 0, TimeSpan.Zero);

    private static ChargingOptions Options() => new()
    {
        MinChargeAmps = 5,
        MaxChargeAmps = 16,
        MaxSocPercent = 80,
        OverrideSettleWindow = TimeSpan.FromMinutes(3),
    };

    private static VehicleStateEntity Vehicle(
        int? soc,
        ChargingState state = ChargingState.Charging,
        bool overrideActive = false,
        DateTimeOffset? socStopIssuedAt = null) => new()
        {
            PartitionKey = Vin,
            RowKey = VehicleStateEntity.StateRowKey,
            ChargingState = state.ToString(),
            BatteryLevelPercent = soc,
            OverrideActive = overrideActive,
            SocStopIssuedAt = socStopIssuedAt,
            LastUpdated = Now.AddMinutes(-2),
        };

    [Theory]
    [InlineData(80)]
    [InlineData(81)]
    [InlineData(100)]
    public void Stops_charging_at_or_above_the_cap(int soc)
    {
        var decision = ChargeDecisionEngine.Decide(Vehicle(soc), maxAmpsLastHour: 16, Options(), Now);

        decision.Action.Should().Be(ChargeAction.StopCharging);
        decision.ShouldStop.Should().BeTrue();
        decision.ShouldSend.Should().BeFalse();
    }

    [Theory]
    [InlineData(0)]
    [InlineData(50)]
    [InlineData(79)]
    public void Charges_normally_below_the_cap(int soc)
    {
        var decision = ChargeDecisionEngine.Decide(Vehicle(soc), maxAmpsLastHour: 12, Options(), Now);

        decision.Action.Should().Be(ChargeAction.SetAmps);
        decision.TargetAmps.Should().Be(12);
    }

    [Fact]
    public void Does_not_send_a_stop_when_already_not_charging_at_the_cap()
    {
        var decision = ChargeDecisionEngine.Decide(
            Vehicle(85, ChargingState.Complete), maxAmpsLastHour: 16, Options(), Now);

        decision.Action.Should().Be(ChargeAction.SkipAtSocCap);
        decision.ShouldStop.Should().BeFalse();
    }

    [Fact]
    public void Ignores_the_cap_when_state_of_charge_is_unknown()
    {
        // Telemetry streams on change, so SoC may be absent early in a session. Refusing to charge
        // on missing data would strand the car; the vehicle's own limit still applies.
        var decision = ChargeDecisionEngine.Decide(Vehicle(null), maxAmpsLastHour: 12, Options(), Now);

        decision.Action.Should().Be(ChargeAction.SetAmps);
    }

    [Fact]
    public void A_manual_override_outranks_the_cap()
    {
        var decision = ChargeDecisionEngine.Decide(
            Vehicle(95, overrideActive: true), maxAmpsLastHour: 16, Options(), Now);

        decision.Action.Should().Be(ChargeAction.SkipOverrideActive);
        decision.ShouldStop.Should().BeFalse();
    }

    [Fact]
    public void A_custom_cap_is_honoured()
    {
        var options = Options();
        options.MaxSocPercent = 60;

        ChargeDecisionEngine.Decide(Vehicle(65), 16, options, Now)
            .Action.Should().Be(ChargeAction.StopCharging);

        ChargeDecisionEngine.Decide(Vehicle(55), 16, options, Now)
            .Action.Should().Be(ChargeAction.SetAmps);
    }

    [Fact]
    public void Restarting_after_our_stop_is_treated_as_a_manual_override()
    {
        // Nothing in this controller restarts a charge, so if the car is charging again after we
        // stopped it, a person did it deliberately.
        var state = Vehicle(85, ChargingState.Stopped, socStopIssuedAt: Now.AddMinutes(-30));

        var result = OverrideEvaluator.Apply(
            state,
            new TelemetryObservation
            {
                Vin = Vin,
                ObservedAt = Now,
                ChargingState = ChargingState.Charging,
            },
            Options());

        result.OverrideActive.Should().BeTrue();
    }

    [Fact]
    public void Telemetry_arriving_just_after_our_stop_is_not_an_override()
    {
        // The stop takes a moment to land; frames still in flight say Charging.
        var state = Vehicle(85, ChargingState.Charging, socStopIssuedAt: Now.AddSeconds(-20));

        var result = OverrideEvaluator.Apply(
            state,
            new TelemetryObservation
            {
                Vin = Vin,
                ObservedAt = Now,
                ChargingState = ChargingState.Charging,
            },
            Options());

        result.OverrideActive.Should().BeFalse();
    }

    [Fact]
    public void Unplugging_clears_the_stop_marker()
    {
        var state = Vehicle(85, ChargingState.Complete, socStopIssuedAt: Now.AddMinutes(-30));

        var result = OverrideEvaluator.Apply(
            state,
            new TelemetryObservation
            {
                Vin = Vin,
                ObservedAt = Now,
                ChargingState = ChargingState.Disconnected,
            },
            Options());

        result.SocStopIssuedAt.Should().BeNull();
        result.OverrideActive.Should().BeFalse();
    }

    [Fact]
    public void State_of_charge_is_recorded_from_telemetry()
    {
        var state = Vehicle(null, ChargingState.Charging);

        var result = OverrideEvaluator.Apply(
            state,
            new TelemetryObservation { Vin = Vin, ObservedAt = Now, BatteryLevelPercent = 72 },
            Options());

        result.BatteryLevelPercent.Should().Be(72);
    }

    [Theory]
    [InlineData(-5)]
    [InlineData(101)]
    public void Implausible_state_of_charge_readings_are_ignored(int soc)
    {
        var state = Vehicle(70, ChargingState.Charging);

        var result = OverrideEvaluator.Apply(
            state,
            new TelemetryObservation { Vin = Vin, ObservedAt = Now, BatteryLevelPercent = soc },
            Options());

        result.BatteryLevelPercent.Should().Be(70);
    }
}
