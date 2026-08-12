using System.Globalization;
using Azure;
using Azure.Data.Tables;
using EvSolarChargeController.Functions.Domain;

namespace EvSolarChargeController.Functions.Storage;

/// <summary>
/// One row per vehicle: the latest known charge state plus what this controller last commanded.
/// PartitionKey = VIN, RowKey = "state".
/// </summary>
public sealed class VehicleStateEntity : ITableEntity
{
    public const string StateRowKey = "state";

    public string PartitionKey { get; set; } = string.Empty;
    public string RowKey { get; set; } = StateRowKey;
    public DateTimeOffset? Timestamp { get; set; }
    public ETag ETag { get; set; }

    /// <summary>VIN. Mirrors <see cref="PartitionKey"/>.</summary>
    public string Vin
    {
        get => PartitionKey;
        set => PartitionKey = value;
    }

    /// <summary>Charge current the vehicle currently reports.</summary>
    public int? ChargeAmps { get; set; }

    /// <summary>Vehicle-reported maximum requestable current, when the car sends it.</summary>
    public int? ReportedMaxAmps { get; set; }

    /// <summary>Battery state of charge, percent, from the vehicle's last report.</summary>
    public int? BatteryLevelPercent { get; set; }

    /// <summary>
    /// When this controller last issued charge_stop because the SoC cap was reached. If the car is
    /// charging again well after this, someone restarted it deliberately.
    /// </summary>
    public DateTimeOffset? SocStopIssuedAt { get; set; }

    /// <summary>
    /// When this controller last stopped charging because solar could not cover the minimum
    /// current. Unlike the SoC cap, this one resumes automatically once production recovers.
    /// </summary>
    public DateTimeOffset? LowSolarStopIssuedAt { get; set; }

    /// <summary>Serialized <see cref="Domain.ChargingState"/>.</summary>
    public string ChargingState { get; set; } = Domain.ChargingState.Unknown.ToString();

    public bool OverrideActive { get; set; }

    public DateTimeOffset? OverrideDetectedAt { get; set; }

    /// <summary>Last value this controller successfully commanded, or null if we have not set one.</summary>
    public int? LastSetAmps { get; set; }

    public DateTimeOffset? LastSetAt { get; set; }

    public DateTimeOffset LastUpdated { get; set; }

    public ChargingState GetChargingState() =>
        Enum.TryParse<ChargingState>(ChargingState, ignoreCase: true, out var parsed)
            ? parsed
            : Domain.ChargingState.Unknown;

    public static VehicleStateEntity CreateNew(string vin, DateTimeOffset now) => new()
    {
        PartitionKey = vin,
        RowKey = StateRowKey,
        ChargingState = Domain.ChargingState.Unknown.ToString(),
        LastUpdated = now,
    };
}

/// <summary>
/// Rolling solar production time series. PartitionKey = local date (yyyy-MM-dd),
/// RowKey = sortable UTC timestamp, so a range query over the partition is naturally ordered.
/// </summary>
public sealed class SolarReadingEntity : ITableEntity
{
    public const string RowKeyFormat = "yyyyMMddTHHmmssfff";

    public string PartitionKey { get; set; } = string.Empty;
    public string RowKey { get; set; } = string.Empty;
    public DateTimeOffset? Timestamp { get; set; }
    public ETag ETag { get; set; }

    public double WattsProduced { get; set; }

    public double AmpsEquivalent { get; set; }

    public DateTimeOffset ReadingAt { get; set; }

    public static SolarReadingEntity Create(DateTimeOffset readingAtUtc, double watts, double amps) => new()
    {
        PartitionKey = PartitionKeyFor(readingAtUtc),
        RowKey = readingAtUtc.UtcDateTime.ToString(RowKeyFormat, CultureInfo.InvariantCulture),
        WattsProduced = watts,
        AmpsEquivalent = amps,
        ReadingAt = readingAtUtc,
    };

    public static string PartitionKeyFor(DateTimeOffset instant) =>
        instant.UtcDateTime.ToString("yyyy-MM-dd", CultureInfo.InvariantCulture);
}

/// <summary>
/// Monthly counter of outbound third-party API calls, so usage can be checked against the
/// Enphase Watt plan's 1000/month cap. PartitionKey = provider, RowKey = yyyy-MM (UTC).
/// </summary>
public sealed class ApiUsageEntity : ITableEntity
{
    public string PartitionKey { get; set; } = string.Empty;
    public string RowKey { get; set; } = string.Empty;
    public DateTimeOffset? Timestamp { get; set; }
    public ETag ETag { get; set; }

    public int CallCount { get; set; }

    public int FailureCount { get; set; }

    public DateTimeOffset LastCallAt { get; set; }

    public static string RowKeyFor(DateTimeOffset instant) =>
        instant.UtcDateTime.ToString("yyyy-MM", CultureInfo.InvariantCulture);
}

/// <summary>Fallback refresh-token storage used when no Key Vault is configured (local dev).</summary>
public sealed class SecretEntity : ITableEntity
{
    public string PartitionKey { get; set; } = "secret";
    public string RowKey { get; set; } = string.Empty;
    public DateTimeOffset? Timestamp { get; set; }
    public ETag ETag { get; set; }

    public string Value { get; set; } = string.Empty;
}
