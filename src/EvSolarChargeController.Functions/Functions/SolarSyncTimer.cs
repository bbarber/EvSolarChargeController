using EvSolarChargeController.Functions.Configuration;
using EvSolarChargeController.Functions.Domain;
using EvSolarChargeController.Functions.Enphase;
using EvSolarChargeController.Functions.Storage;
using EvSolarChargeController.Functions.Tesla;
using Microsoft.Azure.Functions.Worker;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;

namespace EvSolarChargeController.Functions.Functions;

/// <summary>
/// Polls Enphase for current solar production and, when the car is charging and un-overridden,
/// matches the wall connector's amperage to the maximum production seen in the trailing hour.
/// </summary>
/// <remarks>
/// <para>
/// Runs every 20 minutes inside the daylight window only: 3/hour x 10 hours x 31 days = 930 calls,
/// under the Enphase Watt plan's 1000/month cap. Failures are never retried within a run — a
/// skipped cycle costs nothing, but a retry storm can blow the monthly budget.
/// </para>
/// <para>
/// The vehicle is never polled. Charge state comes from pushed telemetry, and no Fleet API call is
/// made unless that telemetry says the car is already awake and charging.
/// </para>
/// </remarks>
public sealed class SolarSyncTimer
{
    // :00, :20, :40 from 09:00 through 18:40 local — the last fire is before the 19:00 cutoff.
    public const string Schedule = "0 0,20,40 9-18 * * *";

    private readonly IEnphaseClient _enphase;
    private readonly ISolarReadingsRepository _readings;
    private readonly IVehicleStateRepository _vehicles;
    private readonly ITeslaFleetClient _tesla;
    private readonly PollingWindow _window;
    private readonly ChargingOptions _charging;
    private readonly TimeProvider _timeProvider;
    private readonly ILogger<SolarSyncTimer> _logger;

    public SolarSyncTimer(
        IEnphaseClient enphase,
        ISolarReadingsRepository readings,
        IVehicleStateRepository vehicles,
        ITeslaFleetClient tesla,
        PollingWindow window,
        IOptions<ChargingOptions> charging,
        TimeProvider timeProvider,
        ILogger<SolarSyncTimer> logger)
    {
        _enphase = enphase;
        _readings = readings;
        _vehicles = vehicles;
        _tesla = tesla;
        _window = window;
        _charging = charging.Value;
        _timeProvider = timeProvider;
        _logger = logger;
    }

    [Function("SolarSyncTimer")]
    public async Task RunAsync(
        [TimerTrigger(Schedule)] TimerInfo timer,
        CancellationToken cancellationToken)
    {
        var now = _timeProvider.GetUtcNow();

        if (!_window.IsOpen(now))
        {
            _logger.LogWarning(
                "Timer fired outside the polling window at {LocalTime}; skipping without spending an Enphase call. " +
                "Check that WEBSITE_TIME_ZONE is set.",
                _window.Describe(now));
            return;
        }

        var production = await PollSolarAsync(now, cancellationToken);

        // Prune and summarize regardless of whether this poll succeeded: an expired window should
        // still shrink, and a failed poll can still act on the readings already banked.
        var summary = await _readings.PruneAndSummarizeAsync(now, _charging.LookbackWindow, cancellationToken);

        _logger.LogInformation(
            "Solar window: {Count} readings, max {MaxAmps:F2}A ({MaxWatts:F0}W), pruned {Pruned}. Latest poll: {PollStatus}.",
            summary.ReadingCount,
            summary.MaxAmps ?? 0,
            summary.MaxWatts ?? 0,
            summary.PrunedCount,
            production is null ? "failed" : $"{production.Watts:F0}W");

        var vehicle = await SelectVehicleAsync(cancellationToken);
        var decision = ChargeDecisionEngine.Decide(vehicle, summary.MaxAmps, _charging, now);

        if (decision.ShouldStop)
        {
            _logger.LogInformation("{Reason}", decision.Reason);
            await StopChargingAsync(vehicle!, now, cancellationToken);
            return;
        }

        if (!decision.ShouldSend)
        {
            _logger.LogInformation("No command sent ({Action}): {Reason}", decision.Action, decision.Reason);
            return;
        }

        _logger.LogInformation("{Reason}", decision.Reason);

        var target = decision.TargetAmps!.Value;
        var result = await _tesla.SetChargingAmpsAsync(vehicle!.Vin, target, cancellationToken);

        if (!result.Success)
        {
            _logger.LogError("set_charging_amps to {Amps}A failed for {Vin}: {Error}", target, vehicle.Vin, result.Error);
            return;
        }

        await _vehicles.MutateAsync(
            vehicle.Vin,
            state =>
            {
                state.LastSetAmps = target;
                state.LastSetAt = now;
                return state;
            },
            now,
            cancellationToken);

        _logger.LogInformation("Charge current for {Vin} set to {Amps}A.", vehicle.Vin, target);
    }

    /// <summary>
    /// Ends the charge session at the state-of-charge cap and records that we did so, which is what
    /// lets a later manual restart be recognised as an override rather than fought every cycle.
    /// </summary>
    private async Task StopChargingAsync(VehicleStateEntity vehicle, DateTimeOffset now, CancellationToken cancellationToken)
    {
        var result = await _tesla.StopChargingAsync(vehicle.Vin, cancellationToken);

        if (!result.Success)
        {
            _logger.LogError("charge_stop failed for {Vin}: {Error}", vehicle.Vin, result.Error);
            return;
        }

        await _vehicles.MutateAsync(
            vehicle.Vin,
            state =>
            {
                state.SocStopIssuedAt = now;
                // Forget our amp setting: the next session starts fresh, and keeping a stale value
                // would make the first telemetry frame of that session look like an override.
                state.LastSetAmps = null;
                state.LastSetAt = null;
                return state;
            },
            now,
            cancellationToken);

        _logger.LogInformation("Charging stopped for {Vin} at the state-of-charge cap.", vehicle.Vin);
    }

    /// <summary>Polls Enphase and banks the reading. Returns null when the cycle should be skipped.</summary>
    private async Task<SolarProduction?> PollSolarAsync(DateTimeOffset now, CancellationToken cancellationToken)
    {
        var result = await _enphase.GetCurrentProductionAsync(now, cancellationToken);

        if (!result.Success)
        {
            _logger.LogWarning("Enphase poll skipped ({Reason}): {Message}", result.Reason, result.Message);
            return null;
        }

        var production = result.Production!;
        var amps = SolarMath.WattsToAmps(production.Watts, _charging.SystemVoltage);

        // Key the row on the current instant rather than Enphase's last_report_at: the reporting
        // timestamp can lag or repeat, which would overwrite a previous sample and shorten the window.
        await _readings.AddAsync(SolarReadingEntity.Create(now, production.Watts, amps), cancellationToken);

        _logger.LogInformation(
            "Solar production {Watts:F0}W -> {Amps:F2}A at {SystemVoltage}V (reported at {ReportedAt:O}).",
            production.Watts,
            amps,
            _charging.SystemVoltage,
            production.ReadingAt);

        return production;
    }

    /// <summary>
    /// Picks the vehicle to act on. One wall connector is shared between two cars, so the plugged-in
    /// one is whichever most recently reported a plugged-in state.
    /// </summary>
    private async Task<VehicleStateEntity?> SelectVehicleAsync(CancellationToken cancellationToken)
    {
        var all = await _vehicles.GetAllAsync(cancellationToken);
        if (all.Count == 0)
        {
            return null;
        }

        var charging = all
            .Where(v => v.GetChargingState().IsActivelyCharging())
            .OrderByDescending(v => v.LastUpdated)
            .ToList();

        if (charging.Count > 1)
        {
            _logger.LogWarning(
                "{Count} vehicles report Charging at once ({Vins}); acting on the most recent. " +
                "A shared connector should only ever have one.",
                charging.Count,
                string.Join(", ", charging.Select(v => v.Vin)));
        }

        return charging.FirstOrDefault()
            ?? all.OrderByDescending(v => v.LastUpdated).First();
    }
}
