using EvSolarChargeController.Functions.Configuration;
using EvSolarChargeController.Functions.Storage;

namespace EvSolarChargeController.Functions.Domain;

public enum ChargeAction
{
    /// <summary>Issue a set_charging_amps command.</summary>
    SetAmps,

    /// <summary>
    /// State of charge has reached the configured cap. Issue charge_stop — this controller does
    /// not charge past it even when the vehicle's own limit is set higher.
    /// </summary>
    StopCharging,

    /// <summary>At or above the SoC cap and already stopped; nothing to do.</summary>
    SkipAtSocCap,

    /// <summary>
    /// Solar cannot sustain even the minimum charge current. Stop rather than clamp up to the
    /// minimum, which would draw the shortfall from the grid.
    /// </summary>
    StopChargingLowSolar,

    /// <summary>Solar has recovered after we stopped for low production; resume the session.</summary>
    ResumeCharging,

    /// <summary>Not enough solar to charge, and not currently charging.</summary>
    SkipInsufficientSolar,

    /// <summary>No telemetry has ever arrived for this vehicle, or it is too old to trust.</summary>
    SkipNoVehicleState,

    /// <summary>Vehicle is not actively charging. Never call Fleet API here — it could wake the car.</summary>
    SkipNotCharging,

    /// <summary>User changed amps in the Tesla app; hands off until the car unplugs.</summary>
    SkipOverrideActive,

    /// <summary>No usable solar readings in the trailing window.</summary>
    SkipNoSolarData,

    /// <summary>Target equals what we already set — sending it again would be a wasted command.</summary>
    SkipAlreadyAtTarget,
}

/// <summary>Outcome of the sync evaluation, with a human-readable reason for App Insights.</summary>
public sealed record ChargeDecision(ChargeAction Action, int? TargetAmps, string Reason)
{
    public bool ShouldSend => Action == ChargeAction.SetAmps;

    public bool ShouldStop => Action is ChargeAction.StopCharging or ChargeAction.StopChargingLowSolar;

    public bool ShouldResume => Action == ChargeAction.ResumeCharging;

    public static ChargeDecision Skip(ChargeAction action, string reason) => new(action, null, reason);

    public static ChargeDecision Set(int amps, string reason) => new(ChargeAction.SetAmps, amps, reason);

    public static ChargeDecision Stop(string reason) => new(ChargeAction.StopCharging, null, reason);

    public static ChargeDecision StopLowSolar(string reason) => new(ChargeAction.StopChargingLowSolar, null, reason);

    public static ChargeDecision Resume(int amps, string reason) => new(ChargeAction.ResumeCharging, amps, reason);
}

/// <summary>
/// Pure decision logic for the solar -> amps sync. Kept free of I/O so the gating rules
/// (never wake a sleeping car, respect manual overrides) are directly testable.
/// </summary>
public static class ChargeDecisionEngine
{
    public static ChargeDecision Decide(
        VehicleStateEntity? vehicle,
        double? maxAmpsLastHour,
        ChargingOptions options,
        DateTimeOffset now)
    {
        ArgumentNullException.ThrowIfNull(options);

        if (vehicle is null)
        {
            return ChargeDecision.Skip(
                ChargeAction.SkipNoVehicleState,
                "No telemetry received yet for any managed vehicle.");
        }

        var age = now - vehicle.LastUpdated;
        if (age > options.VehicleStateStaleAfter)
        {
            return ChargeDecision.Skip(
                ChargeAction.SkipNoVehicleState,
                $"Telemetry for {vehicle.Vin} is {age.TotalHours:F1}h old (stale after {options.VehicleStateStaleAfter.TotalHours:F0}h); assuming asleep.");
        }

        if (vehicle.OverrideActive)
        {
            return ChargeDecision.Skip(
                ChargeAction.SkipOverrideActive,
                $"Manual override active for {vehicle.Vin} since {vehicle.OverrideDetectedAt:O}; waiting for unplug.");
        }

        var state = vehicle.GetChargingState();

        // The SoC cap is checked before the charging test so that an already-stopped car at or
        // above the cap reports the real reason rather than a generic "not charging".
        if (vehicle.BatteryLevelPercent is { } soc && soc >= options.MaxSocPercent)
        {
            if (!state.IsActivelyCharging())
            {
                return ChargeDecision.Skip(
                    ChargeAction.SkipAtSocCap,
                    $"{vehicle.Vin} is at {soc}% (cap {options.MaxSocPercent}%) and not charging.");
            }

            return ChargeDecision.Stop(
                $"{vehicle.Vin} reached {soc}%, at or above the {options.MaxSocPercent}% cap; stopping the charge session.");
        }

        if (maxAmpsLastHour is not { } amps)
        {
            return ChargeDecision.Skip(
                ChargeAction.SkipNoSolarData,
                "No solar readings inside the trailing window; leaving amps unchanged.");
        }

        // Whether solar alone can sustain the connector's minimum current. Rounded before the
        // clamp, because clamping *up* to the minimum is exactly the grid draw we want to avoid.
        var unclamped = (int)Math.Round(amps, MidpointRounding.AwayFromZero);
        var solarCoversMinimum = unclamped >= options.MinChargeAmps;

        if (!solarCoversMinimum)
        {
            // Taking the maximum over the trailing hour already damps this heavily: a passing
            // cloud cannot trip it, only a sustained loss of production.
            if (state.IsActivelyCharging())
            {
                return ChargeDecision.StopLowSolar(
                    $"Solar peaked at {amps:F2}A over the trailing window, below the {options.MinChargeAmps}A minimum; " +
                    $"stopping {vehicle.Vin} rather than drawing the shortfall from the grid.");
            }

            return ChargeDecision.Skip(
                ChargeAction.SkipInsufficientSolar,
                $"Solar peaked at {amps:F2}A, below the {options.MinChargeAmps}A minimum; leaving {vehicle.Vin} stopped.");
        }

        var target = SolarMath.ToRequestableAmps(amps, options.MinChargeAmps, options.MaxChargeAmps);

        if (!state.IsActivelyCharging())
        {
            // Only resume a session this controller stopped for low solar. Anything else that is
            // not charging is either the user's choice or an asleep vehicle, and a command here
            // could wake it.
            if (vehicle.LowSolarStopIssuedAt is not null && state.IsPluggedIn())
            {
                return ChargeDecision.Resume(
                    target,
                    $"Solar recovered to {amps:F2}A; resuming {vehicle.Vin} at {target}A.");
            }

            return ChargeDecision.Skip(
                ChargeAction.SkipNotCharging,
                $"{vehicle.Vin} is {state}; not sending any command (avoids waking the vehicle).");
        }

        // The car may cap below our configured ceiling (breaker size, on-board charger). Respect
        // whatever it last reported so we stop asking for current it will never accept — otherwise
        // every cycle would look like a mismatch and trip false override detection.
        if (vehicle.ReportedMaxAmps is { } vehicleMax && vehicleMax >= options.MinChargeAmps && target > vehicleMax)
        {
            target = vehicleMax;
        }

        if (vehicle.LastSetAmps == target)
        {
            return ChargeDecision.Skip(
                ChargeAction.SkipAlreadyAtTarget,
                $"Target {target}A already set for {vehicle.Vin}.");
        }

        return ChargeDecision.Set(
            target,
            $"Solar max over trailing window {amps:F2}A -> requesting {target}A for {vehicle.Vin} (was {vehicle.LastSetAmps?.ToString() ?? "unset"}).");
    }
}
