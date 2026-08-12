using EvSolarChargeController.Functions.Configuration;
using EvSolarChargeController.Functions.Domain;
using EvSolarChargeController.Functions.Storage;
using FluentAssertions;
using Xunit;

namespace EvSolarChargeController.Tests;

/// <summary>
/// When solar cannot cover the connector's minimum current, clamping up to that minimum would draw
/// the shortfall from the grid — the opposite of the point. The session is stopped instead, and
/// resumed once production recovers.
/// </summary>
public class LowSolarTests
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
        ChargingState state = ChargingState.Charging,
        DateTimeOffset? lowSolarStopIssuedAt = null,
        int? soc = 50,
        bool overrideActive = false) => new()
        {
            PartitionKey = Vin,
            RowKey = VehicleStateEntity.StateRowKey,
            ChargingState = state.ToString(),
            BatteryLevelPercent = soc,
            LowSolarStopIssuedAt = lowSolarStopIssuedAt,
            OverrideActive = overrideActive,
            LastUpdated = Now.AddMinutes(-2),
        };

    [Theory]
    [InlineData(0)]
    [InlineData(1.2)]
    [InlineData(4.4)]
    public void Stops_rather_than_charging_from_the_grid(double amps)
    {
        var decision = ChargeDecisionEngine.Decide(Vehicle(), amps, Options(), Now);

        decision.Action.Should().Be(ChargeAction.StopChargingLowSolar);
        decision.ShouldStop.Should().BeTrue();
    }

    [Fact]
    public void Charges_when_solar_rounds_up_to_the_minimum()
    {
        // 4.6A of production against a 5A request is ~96W from the grid — not worth stopping over.
        var decision = ChargeDecisionEngine.Decide(Vehicle(), 4.6, Options(), Now);

        decision.Action.Should().Be(ChargeAction.SetAmps);
        decision.TargetAmps.Should().Be(5);
    }

    [Fact]
    public void Stays_stopped_while_solar_is_low()
    {
        var decision = ChargeDecisionEngine.Decide(
            Vehicle(ChargingState.Stopped, lowSolarStopIssuedAt: Now.AddHours(-1)), 2, Options(), Now);

        decision.Action.Should().Be(ChargeAction.SkipInsufficientSolar);
        decision.ShouldStop.Should().BeFalse();
        decision.ShouldResume.Should().BeFalse();
    }

    [Fact]
    public void Resumes_once_solar_recovers()
    {
        var decision = ChargeDecisionEngine.Decide(
            Vehicle(ChargingState.Stopped, lowSolarStopIssuedAt: Now.AddHours(-1)), 11, Options(), Now);

        decision.Action.Should().Be(ChargeAction.ResumeCharging);
        decision.ShouldResume.Should().BeTrue();
        decision.TargetAmps.Should().Be(11);
    }

    [Fact]
    public void Does_not_resume_a_session_this_controller_did_not_stop()
    {
        // The user stopped it, or the car simply is not charging. Sending a command here could
        // wake a sleeping vehicle.
        var decision = ChargeDecisionEngine.Decide(
            Vehicle(ChargingState.Stopped, lowSolarStopIssuedAt: null), 12, Options(), Now);

        decision.Action.Should().Be(ChargeAction.SkipNotCharging);
    }

    [Fact]
    public void Does_not_resume_an_unplugged_vehicle()
    {
        var decision = ChargeDecisionEngine.Decide(
            Vehicle(ChargingState.Disconnected, lowSolarStopIssuedAt: Now.AddHours(-1)), 12, Options(), Now);

        decision.Action.Should().Be(ChargeAction.SkipNotCharging);
    }

    [Fact]
    public void Does_not_resume_while_an_override_is_active()
    {
        var decision = ChargeDecisionEngine.Decide(
            Vehicle(ChargingState.Stopped, lowSolarStopIssuedAt: Now.AddHours(-1), overrideActive: true),
            12,
            Options(),
            Now);

        decision.Action.Should().Be(ChargeAction.SkipOverrideActive);
    }

    [Fact]
    public void The_soc_cap_outranks_a_low_solar_resume()
    {
        var decision = ChargeDecisionEngine.Decide(
            Vehicle(ChargingState.Stopped, lowSolarStopIssuedAt: Now.AddHours(-1), soc: 90),
            12,
            Options(),
            Now);

        decision.Action.Should().Be(ChargeAction.SkipAtSocCap);
    }

    [Fact]
    public void Restarting_by_hand_after_a_low_solar_stop_is_an_override()
    {
        // A resume by this controller clears the marker first, so a still-set marker plus a
        // charging car means a person restarted it.
        var state = Vehicle(ChargingState.Stopped, lowSolarStopIssuedAt: Now.AddMinutes(-30));

        var result = OverrideEvaluator.Apply(
            state,
            new TelemetryObservation { Vin = Vin, ObservedAt = Now, ChargingState = ChargingState.Charging },
            Options());

        result.OverrideActive.Should().BeTrue();
    }

    [Fact]
    public void Unplugging_clears_the_low_solar_marker()
    {
        var state = Vehicle(ChargingState.Stopped, lowSolarStopIssuedAt: Now.AddMinutes(-30));

        var result = OverrideEvaluator.Apply(
            state,
            new TelemetryObservation { Vin = Vin, ObservedAt = Now, ChargingState = ChargingState.Disconnected },
            Options());

        result.LowSolarStopIssuedAt.Should().BeNull();
    }

    [Fact]
    public void Missing_solar_data_does_not_stop_a_running_session()
    {
        // A failed Enphase poll is not evidence of low production.
        var decision = ChargeDecisionEngine.Decide(Vehicle(), maxAmpsLastHour: null, Options(), Now);

        decision.Action.Should().Be(ChargeAction.SkipNoSolarData);
        decision.ShouldStop.Should().BeFalse();
    }
}
