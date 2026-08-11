using EvSolarChargeController.Functions.Configuration;
using EvSolarChargeController.Functions.Storage;

namespace EvSolarChargeController.Functions.Domain;

/// <summary>A telemetry observation reduced to just the fields the override rule needs.</summary>
public sealed record TelemetryObservation
{
    public required string Vin { get; init; }

    public required DateTimeOffset ObservedAt { get; init; }

    /// <summary>Amps the vehicle reports as its configured charge current (Tesla field <c>ChargeAmps</c>).</summary>
    public int? ReportedAmps { get; init; }

    /// <summary>Vehicle-reported ceiling (Tesla field <c>ChargeCurrentRequestMax</c>), when present.</summary>
    public int? ReportedMaxAmps { get; init; }

    public ChargingState? ChargingState { get; init; }

    /// <summary>True when the charge port latch reports disengaged — a second unplug signal.</summary>
    public bool? LatchDisengaged { get; init; }
}

/// <summary>
/// Applies the manual-override rule: if the car reports a charge current we did not set,
/// the user changed it in the app, so auto-adjustment stops until the car unplugs.
/// </summary>
public static class OverrideEvaluator
{
    /// <summary>
    /// Folds an observation into the stored state. Returns the updated entity; the caller persists it.
    /// Fields absent from the observation leave the previous value intact, because Tesla streams each
    /// signal on change rather than sending a complete snapshot every time.
    /// </summary>
    public static VehicleStateEntity Apply(
        VehicleStateEntity state,
        TelemetryObservation observation,
        ChargingOptions options)
    {
        ArgumentNullException.ThrowIfNull(state);
        ArgumentNullException.ThrowIfNull(observation);
        ArgumentNullException.ThrowIfNull(options);

        if (observation.ChargingState is { } incomingState)
        {
            state.ChargingState = incomingState.ToString();
        }

        if (observation.ReportedMaxAmps is { } max and > 0)
        {
            state.ReportedMaxAmps = max;
        }

        if (observation.ReportedAmps is { } reported)
        {
            state.ChargeAmps = reported;
        }

        var effectiveState = state.GetChargingState();
        var unplugged = effectiveState.IsUnplugged() || observation.LatchDisengaged == true;

        if (unplugged)
        {
            // Unplug is the reset point for the whole state machine: clear the override and forget
            // what we last set, since the next session starts from whatever the car defaults to.
            state.OverrideActive = false;
            state.OverrideDetectedAt = null;
            state.LastSetAmps = null;
            state.LastSetAt = null;
        }
        else if (ShouldFlagOverride(state, observation, options, effectiveState))
        {
            state.OverrideActive = true;
            state.OverrideDetectedAt = observation.ObservedAt;
        }

        state.LastUpdated = observation.ObservedAt;
        return state;
    }

    private static bool ShouldFlagOverride(
        VehicleStateEntity state,
        TelemetryObservation observation,
        ChargingOptions options,
        ChargingState effectiveState)
    {
        if (state.OverrideActive)
        {
            return false; // Already flagged; nothing to re-evaluate until unplug.
        }

        if (observation.ReportedAmps is not { } reported)
        {
            return false; // This observation carried no amp value.
        }

        if (state.LastSetAmps is not { } lastSet)
        {
            return false; // We have never set a value, so nothing to contradict.
        }

        if (reported == lastSet)
        {
            return false;
        }

        // Only a charging vehicle can meaningfully contradict us; a car that is stopped or
        // complete reports transitional values we should not read as user intent.
        if (!effectiveState.IsActivelyCharging())
        {
            return false;
        }

        // Telemetry sent before our command landed still carries the old value. Ignore mismatches
        // until the command has had time to settle.
        if (state.LastSetAt is { } setAt && observation.ObservedAt - setAt < options.OverrideSettleWindow)
        {
            return false;
        }

        return true;
    }
}
