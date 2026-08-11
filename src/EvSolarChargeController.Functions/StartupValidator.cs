using EvSolarChargeController.Functions.Configuration;
using EvSolarChargeController.Functions.Domain;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;

namespace EvSolarChargeController.Functions;

/// <summary>
/// Surfaces misconfiguration at startup rather than at 9am when the first timer fires.
/// Logs instead of throwing, so a partially configured deployment can still accept telemetry.
/// </summary>
internal static class StartupValidator
{
    public static void Validate(IServiceProvider services, ILogger logger)
    {
        var enphase = services.GetRequiredService<IOptions<EnphaseOptions>>().Value;
        var tesla = services.GetRequiredService<IOptions<TeslaOptions>>().Value;
        var ingest = services.GetRequiredService<IOptions<IngestOptions>>().Value;
        var charging = services.GetRequiredService<IOptions<ChargingOptions>>().Value;

        var missing = new List<string>();

        if (string.IsNullOrWhiteSpace(enphase.ClientId)) missing.Add("Enphase:ClientId");
        if (string.IsNullOrWhiteSpace(enphase.ClientSecret)) missing.Add("Enphase:ClientSecret");
        if (string.IsNullOrWhiteSpace(enphase.ApiKey)) missing.Add("Enphase:ApiKey");
        if (string.IsNullOrWhiteSpace(enphase.SystemId)) missing.Add("Enphase:SystemId");
        if (string.IsNullOrWhiteSpace(tesla.ClientId)) missing.Add("Tesla:ClientId");
        if (string.IsNullOrWhiteSpace(ingest.SharedSecret)) missing.Add("Ingest:SharedSecret");

        if (tesla.CommandMode == TeslaCommandMode.Proxy && string.IsNullOrWhiteSpace(tesla.CommandProxyBaseUrl))
        {
            missing.Add("Tesla:CommandProxyBaseUrl (required when Tesla:CommandMode=Proxy)");
        }

        if (missing.Count > 0)
        {
            logger.LogError(
                "Missing required configuration: {Missing}. See docs/SETUP.md.",
                string.Join(", ", missing));
        }

        if (charging.MinChargeAmps > charging.MaxChargeAmps)
        {
            logger.LogError(
                "Charging:MinChargeAmps ({Min}) exceeds Charging:MaxChargeAmps ({Max}); every sync will fail.",
                charging.MinChargeAmps,
                charging.MaxChargeAmps);
        }

        var window = services.GetRequiredService<PollingWindow>();
        logger.LogInformation(
            "Startup OK. Timer schedule '{Schedule}' interpreted in {TimeZone}; charge range {Min}-{Max}A; Tesla command mode {CommandMode}.",
            Functions.SolarSyncTimer.Schedule,
            window.TimeZoneId,
            charging.MinChargeAmps,
            charging.MaxChargeAmps,
            tesla.CommandMode);
    }
}
