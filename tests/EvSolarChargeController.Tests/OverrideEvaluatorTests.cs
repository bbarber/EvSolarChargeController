using EvSolarChargeController.Functions.Configuration;
using EvSolarChargeController.Functions.Domain;
using EvSolarChargeController.Functions.Storage;
using FluentAssertions;
using Xunit;

namespace EvSolarChargeController.Tests;

public class OverrideEvaluatorTests
{
    private const string Vin = "5YJ3E1EA7KF000001";

    private static readonly DateTimeOffset Now = new(2026, 8, 11, 14, 0, 0, TimeSpan.Zero);

    private static ChargingOptions Options() => new()
    {
        OverrideSettleWindow = TimeSpan.FromMinutes(3),
    };

    private static VehicleStateEntity State(
        int? lastSetAmps = null,
        DateTimeOffset? lastSetAt = null,
        bool overrideActive = false,
        ChargingState state = ChargingState.Charging) => new()
        {
            PartitionKey = Vin,
            RowKey = VehicleStateEntity.StateRowKey,
            LastSetAmps = lastSetAmps,
            LastSetAt = lastSetAt,
            OverrideActive = overrideActive,
            ChargingState = state.ToString(),
            LastUpdated = Now.AddMinutes(-10),
        };

    private static TelemetryObservation Observation(
        int? amps = null,
        ChargingState? state = null,
        bool? latchDisengaged = null,
        DateTimeOffset? at = null,
        int? maxAmps = null) => new()
        {
            Vin = Vin,
            ObservedAt = at ?? Now,
            ReportedAmps = amps,
            ChargingState = state,
            LatchDisengaged = latchDisengaged,
            ReportedMaxAmps = maxAmps,
        };

    [Fact]
    public void Flags_an_override_when_reported_amps_contradict_what_we_set()
    {
        var state = State(lastSetAmps: 12, lastSetAt: Now.AddMinutes(-30));

        var result = OverrideEvaluator.Apply(state, Observation(amps: 32, state: ChargingState.Charging), Options());

        result.OverrideActive.Should().BeTrue();
        result.OverrideDetectedAt.Should().Be(Now);
        result.ChargeAmps.Should().Be(32);
    }

    [Fact]
    public void Does_not_flag_when_the_car_confirms_our_value()
    {
        var state = State(lastSetAmps: 12, lastSetAt: Now.AddMinutes(-30));

        var result = OverrideEvaluator.Apply(state, Observation(amps: 12, state: ChargingState.Charging), Options());

        result.OverrideActive.Should().BeFalse();
    }

    [Fact]
    public void Does_not_flag_inside_the_settle_window()
    {
        // Telemetry emitted before our command landed still carries the previous value.
        var state = State(lastSetAmps: 12, lastSetAt: Now.AddSeconds(-30));

        var result = OverrideEvaluator.Apply(state, Observation(amps: 8, state: ChargingState.Charging), Options());

        result.OverrideActive.Should().BeFalse();
    }

    [Fact]
    public void Does_not_flag_before_we_have_ever_set_a_value()
    {
        var state = State(lastSetAmps: null);

        var result = OverrideEvaluator.Apply(state, Observation(amps: 24, state: ChargingState.Charging), Options());

        result.OverrideActive.Should().BeFalse();
    }

    [Fact]
    public void Does_not_flag_when_the_vehicle_is_not_charging()
    {
        var state = State(lastSetAmps: 12, lastSetAt: Now.AddMinutes(-30));

        var result = OverrideEvaluator.Apply(state, Observation(amps: 8, state: ChargingState.Stopped), Options());

        result.OverrideActive.Should().BeFalse();
    }

    [Fact]
    public void Unplugging_clears_the_override_and_forgets_the_last_set_value()
    {
        var state = State(lastSetAmps: 12, lastSetAt: Now.AddMinutes(-30), overrideActive: true);

        var result = OverrideEvaluator.Apply(state, Observation(state: ChargingState.Disconnected), Options());

        result.OverrideActive.Should().BeFalse();
        result.OverrideDetectedAt.Should().BeNull();
        result.LastSetAmps.Should().BeNull();
        result.LastSetAt.Should().BeNull();
    }

    [Fact]
    public void A_disengaged_latch_also_clears_the_override()
    {
        var state = State(lastSetAmps: 12, overrideActive: true, state: ChargingState.Stopped);

        var result = OverrideEvaluator.Apply(state, Observation(latchDisengaged: true), Options());

        result.OverrideActive.Should().BeFalse();
    }

    [Fact]
    public void An_override_survives_a_pause_in_charging()
    {
        // Charging stopping is not unplugging — the user's setting should still be respected.
        var state = State(lastSetAmps: 12, overrideActive: true);

        var result = OverrideEvaluator.Apply(state, Observation(state: ChargingState.Stopped), Options());

        result.OverrideActive.Should().BeTrue();
    }

    [Fact]
    public void An_override_is_not_re_evaluated_while_already_active()
    {
        var state = State(lastSetAmps: 12, lastSetAt: Now.AddHours(-1), overrideActive: true);
        state.OverrideDetectedAt = Now.AddMinutes(-20);

        var result = OverrideEvaluator.Apply(state, Observation(amps: 20, state: ChargingState.Charging), Options());

        result.OverrideActive.Should().BeTrue();
        result.OverrideDetectedAt.Should().Be(Now.AddMinutes(-20));
    }

    [Fact]
    public void Absent_fields_leave_previous_values_intact()
    {
        // Tesla streams signals on change, so a payload carrying only one field must not wipe others.
        var state = State(lastSetAmps: 12, state: ChargingState.Charging);
        state.ChargeAmps = 12;
        state.ReportedMaxAmps = 16;

        var result = OverrideEvaluator.Apply(state, Observation(maxAmps: null, amps: null), Options());

        result.ChargeAmps.Should().Be(12);
        result.ReportedMaxAmps.Should().Be(16);
        result.GetChargingState().Should().Be(ChargingState.Charging);
    }

    [Fact]
    public void Records_the_observation_time()
    {
        var state = State();
        var observedAt = Now.AddMinutes(-1);

        var result = OverrideEvaluator.Apply(state, Observation(at: observedAt, state: ChargingState.Charging), Options());

        result.LastUpdated.Should().Be(observedAt);
    }

    [Fact]
    public void A_full_session_runs_override_then_reset()
    {
        var options = Options();
        var state = State(lastSetAmps: 10, lastSetAt: Now.AddMinutes(-30));

        // User bumps amps in the app.
        state = OverrideEvaluator.Apply(state, Observation(amps: 32, state: ChargingState.Charging), options);
        state.OverrideActive.Should().BeTrue();

        // Charging completes — override must survive, since the cable is still in.
        state = OverrideEvaluator.Apply(state, Observation(state: ChargingState.Complete, at: Now.AddHours(1)), options);
        state.OverrideActive.Should().BeTrue();

        // Cable comes out — back under automatic control.
        state = OverrideEvaluator.Apply(state, Observation(state: ChargingState.Disconnected, at: Now.AddHours(2)), options);
        state.OverrideActive.Should().BeFalse();
        state.LastSetAmps.Should().BeNull();
    }
}
