using EvSolarChargeController.TelemetryBridge;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;

var builder = Host.CreateApplicationBuilder(args);

// Container Apps supplies configuration as BRIDGE__* environment variables.
builder.Services.Configure<BridgeOptions>(builder.Configuration.GetSection(BridgeOptions.SectionName));

builder.Services.AddHttpClient("ingest", client =>
{
    client.Timeout = TimeSpan.FromSeconds(15);
    client.DefaultRequestHeaders.UserAgent.ParseAdd("EvSolarChargeController.TelemetryBridge/1.0");
});

builder.Services.AddHostedService<TelemetryBridgeService>();

builder.Logging.AddSimpleConsole(options =>
{
    options.SingleLine = true;
    options.TimestampFormat = "yyyy-MM-ddTHH:mm:ssZ ";
    options.UseUtcTimestamp = true;
});

await builder.Build().RunAsync();
