namespace EvSolarChargeController.Functions.Configuration;

/// <summary>
/// Electrical + charging limits used to translate solar production into a charge current.
/// </summary>
public sealed class ChargingOptions
{
    public const string SectionName = "Charging";

    /// <summary>Service voltage used for the watts -> amps conversion. US residential split-phase is 240V.</summary>
    public double SystemVoltage { get; set; } = 240d;

    /// <summary>Lowest current the wall connector / vehicle will accept. Below this we still request the minimum.</summary>
    public int MinChargeAmps { get; set; } = 5;

    /// <summary>
    /// Upper bound we will ever request, regardless of solar production. Set to 16A to match the
    /// array's peak output — asking for more would only ever pull the difference from the grid.
    /// </summary>
    public int MaxChargeAmps { get; set; } = 16;

    /// <summary>Trailing window used for the "max amps seen recently" calculation.</summary>
    public TimeSpan LookbackWindow { get; set; } = TimeSpan.FromMinutes(60);

    /// <summary>
    /// After we issue a set_charging_amps we ignore reported-amps mismatches for this long, so that
    /// in-flight telemetry carrying the previous value is not misread as a manual override.
    /// </summary>
    public TimeSpan OverrideSettleWindow { get; set; } = TimeSpan.FromMinutes(3);

    /// <summary>Telemetry older than this is treated as unknown rather than authoritative.</summary>
    public TimeSpan VehicleStateStaleAfter { get; set; } = TimeSpan.FromHours(6);
}

/// <summary>
/// Enphase Enlighten (API v4) credentials and rate-limit budget for the free "Watt" plan.
/// </summary>
public sealed class EnphaseOptions
{
    public const string SectionName = "Enphase";

    public string BaseUrl { get; set; } = "https://api.enphaseenergy.com/api/v4";
    public string TokenUrl { get; set; } = "https://api.enphaseenergy.com/oauth/token";

    public string ClientId { get; set; } = string.Empty;
    public string ClientSecret { get; set; } = string.Empty;
    public string ApiKey { get; set; } = string.Empty;
    public string SystemId { get; set; } = string.Empty;

    /// <summary>
    /// Hard monthly ceiling on Enphase calls. The Watt plan allows 1000/month; we stop short of that
    /// so a retried deploy or a 31-day month cannot push us over.
    /// </summary>
    public int MonthlyCallBudget { get; set; } = 950;

    /// <summary>Refresh the access token when it is within this window of expiring.</summary>
    public TimeSpan TokenRefreshSkew { get; set; } = TimeSpan.FromMinutes(30);
}

/// <summary>
/// Daylight-only polling window. Outside this window <c>SolarSyncTimer</c> exits without
/// spending an Enphase call. This is enforced in code as well as in the NCRONTAB expression,
/// so a missing WEBSITE_TIME_ZONE cannot silently burn the monthly budget overnight.
/// </summary>
public sealed class PollingWindowOptions
{
    public const string SectionName = "PollingWindow";

    public string TimeZone { get; set; } = "America/Chicago";
    public int StartHourLocal { get; set; } = 9;
    public int EndHourLocal { get; set; } = 19;
}

/// <summary>How vehicle commands reach the car.</summary>
public enum TeslaCommandMode
{
    /// <summary>
    /// POST straight to Fleet API. Only works for pre-2021 Model S/X and business-owned fleet
    /// vehicles; every other car rejects unsigned commands.
    /// </summary>
    Direct,

    /// <summary>
    /// POST to a tesla-http-proxy instance which signs the command with the app's private key.
    /// Required for all 2021+ vehicles.
    /// </summary>
    Proxy,
}

public sealed class TeslaOptions
{
    public const string SectionName = "Tesla";

    /// <summary>Region-specific Fleet API host, e.g. https://fleet-api.prd.na.vn.cloud.tesla.com</summary>
    public string FleetApiBaseUrl { get; set; } = "https://fleet-api.prd.na.vn.cloud.tesla.com";

    public string TokenUrl { get; set; } = "https://fleet-auth.prd.vn.cloud.tesla.com/oauth2/v3/token";

    public string ClientId { get; set; } = string.Empty;
    public string ClientSecret { get; set; } = string.Empty;

    /// <summary>Scopes requested on refresh. Must match what the app registration was granted.</summary>
    public string Scopes { get; set; } = "openid offline_access vehicle_device_data vehicle_cmds vehicle_charging_cmds";

    public TeslaCommandMode CommandMode { get; set; } = TeslaCommandMode.Proxy;

    /// <summary>Base URL of the tesla-http-proxy container app. Required when <see cref="CommandMode"/> is Proxy.</summary>
    public string CommandProxyBaseUrl { get; set; } = string.Empty;

    /// <summary>Shared secret header the proxy front-end requires, so the public ingress is not open to all.</summary>
    public string CommandProxySharedSecret { get; set; } = string.Empty;

    /// <summary>
    /// VINs this controller manages. One wall connector is shared, so at most one is expected to be
    /// plugged in at a time; the sync picks whichever VIN last reported a charging state.
    /// </summary>
    public string[] Vins { get; set; } = Array.Empty<string>();

    public TimeSpan TokenRefreshSkew { get; set; } = TimeSpan.FromMinutes(5);
}

/// <summary>Secrets shared between APIM and the Function so the ingest endpoint cannot be called directly.</summary>
public sealed class IngestOptions
{
    public const string SectionName = "Ingest";

    public const string SharedSecretHeaderName = "X-Ingest-Secret";

    public string SharedSecret { get; set; } = string.Empty;
}

/// <summary>Where rotating OAuth refresh tokens are persisted.</summary>
public sealed class SecretStoreOptions
{
    public const string SectionName = "SecretStore";

    /// <summary>Key Vault URI. When empty, tokens fall back to the Table Storage store (local dev).</summary>
    public string KeyVaultUri { get; set; } = string.Empty;

    public string EnphaseRefreshTokenSecretName { get; set; } = "enphase-refresh-token";
    public string TeslaRefreshTokenSecretName { get; set; } = "tesla-refresh-token";
}
