using EvSolarChargeController.Functions.Configuration;
using EvSolarChargeController.Functions.Storage;

namespace EvSolarChargeController.Functions.Domain;

public enum ChargeAction
{
    /// <summary>Issue a set_charging_amps command.</summary>
    SetAmps,

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

    public static ChargeDecision Skip(ChargeAction action, string reason) => new(action, null, reason);

    public static ChargeDecision Set(int amps, string reason) => new(ChargeAction.SetAmps, amps, reason);
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
        if (!state.IsActivelyCharging())
        {
            return ChargeDecision.Skip(
                ChargeAction.SkipNotCharging,
                $"{vehicle.Vin} is {state}; not sending any command (avoids waking the vehicle).");
        }

        if (maxAmpsLastHour is not { } amps)
        {
            return ChargeDecision.Skip(
                ChargeAction.SkipNoSolarData,
                "No solar readings inside the trailing window; leaving amps unchanged.");
        }

        var target = SolarMath.ToRequestableAmps(amps, options.MinChargeAmps, options.MaxChargeAmps);

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
