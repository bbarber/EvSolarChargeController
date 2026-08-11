using Azure.Data.Tables;
using Azure.Identity;
using Azure.Security.KeyVault.Secrets;
using EvSolarChargeController.Functions;
using EvSolarChargeController.Functions.Configuration;
using EvSolarChargeController.Functions.Domain;
using EvSolarChargeController.Functions.Enphase;
using EvSolarChargeController.Functions.Storage;
using EvSolarChargeController.Functions.Tesla;
using Microsoft.Azure.Functions.Worker;
using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;

var host = new HostBuilder()
    .ConfigureFunctionsWebApplication()
    .ConfigureServices((context, services) =>
    {
        var configuration = context.Configuration;

        services
            .AddApplicationInsightsTelemetryWorkerService()
            .ConfigureFunctionsApplicationInsights();

        // Section names map to double-underscore app settings, e.g. Charging__SystemVoltage.
        services.Configure<ChargingOptions>(configuration.GetSection(ChargingOptions.SectionName));
        services.Configure<EnphaseOptions>(configuration.GetSection(EnphaseOptions.SectionName));
        services.Configure<TeslaOptions>(configuration.GetSection(TeslaOptions.SectionName));
        services.Configure<PollingWindowOptions>(configuration.GetSection(PollingWindowOptions.SectionName));
        services.Configure<IngestOptions>(configuration.GetSection(IngestOptions.SectionName));
        services.Configure<SecretStoreOptions>(configuration.GetSection(SecretStoreOptions.SectionName));

        services.AddSingleton(TimeProvider.System);

        services.AddSingleton(sp =>
            new PollingWindow(sp.GetRequiredService<IOptions<PollingWindowOptions>>().Value));

        // Prefer managed identity + service URI in production so no storage key is ever stored;
        // fall back to a connection string for local development against Azurite.
        services.AddSingleton(_ =>
        {
            var accountUri = configuration["Storage:ServiceUri"];
            if (!string.IsNullOrWhiteSpace(accountUri))
            {
                return new TableServiceClient(new Uri(accountUri), new DefaultAzureCredential());
            }

            var connectionString = configuration["AzureWebJobsStorage"]
                ?? throw new InvalidOperationException(
                    "Neither Storage:ServiceUri nor AzureWebJobsStorage is configured.");

            return new TableServiceClient(connectionString);
        });

        services.AddSingleton<IVehicleStateRepository, VehicleStateRepository>();
        services.AddSingleton<ISolarReadingsRepository, SolarReadingsRepository>();
        services.AddSingleton<IApiUsageRepository, ApiUsageRepository>();

        // Only resolved when SecretStore:KeyVaultUri is set; the token store checks first.
        services.AddSingleton(sp =>
        {
            var options = sp.GetRequiredService<IOptions<SecretStoreOptions>>().Value;
            if (string.IsNullOrWhiteSpace(options.KeyVaultUri))
            {
                throw new InvalidOperationException("SecretStore:KeyVaultUri is not configured.");
            }

            return new SecretClient(new Uri(options.KeyVaultUri), new DefaultAzureCredential());
        });

        services.AddSingleton<IRefreshTokenStore>(sp =>
        {
            var secretOptions = sp.GetRequiredService<IOptions<SecretStoreOptions>>();

            // Initial refresh tokens arrive as app settings. After the first use the rotated value
            // lives in the store, because both providers invalidate the old token on refresh.
            var seeds = new Dictionary<string, string>(StringComparer.Ordinal);

            var enphaseSeed = configuration["Enphase:RefreshToken"];
            if (!string.IsNullOrWhiteSpace(enphaseSeed))
            {
                seeds[secretOptions.Value.EnphaseRefreshTokenSecretName] = enphaseSeed;
            }

            var teslaSeed = configuration["Tesla:RefreshToken"];
            if (!string.IsNullOrWhiteSpace(teslaSeed))
            {
                seeds[secretOptions.Value.TeslaRefreshTokenSecretName] = teslaSeed;
            }

            return RefreshTokenStoreFactory.Create(sp, secretOptions, seeds);
        });

        services.AddSingleton<ITelemetryDecoder, TelemetryDecoder>();

        services.AddHttpClient<IEnphaseClient, EnphaseClient>(client =>
        {
            client.Timeout = TimeSpan.FromSeconds(30);
            client.DefaultRequestHeaders.UserAgent.ParseAdd("EvSolarChargeController/1.0");
        });

        services.AddHttpClient<ITeslaFleetClient, TeslaFleetClient>(client =>
        {
            client.Timeout = TimeSpan.FromSeconds(30);
            client.DefaultRequestHeaders.UserAgent.ParseAdd("EvSolarChargeController/1.0");
        });
    })
    .Build();

StartupValidator.Validate(
    host.Services,
    host.Services.GetRequiredService<ILoggerFactory>().CreateLogger("Startup"));

await host.RunAsync();
