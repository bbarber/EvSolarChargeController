using System.Net;
using System.Security.Cryptography;
using System.Text;
using EvSolarChargeController.Functions.Configuration;
using EvSolarChargeController.Functions.Domain;
using EvSolarChargeController.Functions.Storage;
using EvSolarChargeController.Functions.Tesla;
using Microsoft.AspNetCore.Http;
using Microsoft.AspNetCore.Mvc;
using Microsoft.Azure.Functions.Worker;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;

namespace EvSolarChargeController.Functions.Functions;

/// <summary>
/// Receives Tesla Fleet Telemetry records and folds them into <c>VehicleState</c>.
/// </summary>
/// <remarks>
/// Callers are the telemetry bridge sidecar (see <c>src/EvSolarChargeController.TelemetryBridge</c>),
/// which forwards raw protobuf payloads out of the fleet-telemetry server. mTLS against Tesla's CA
/// happens upstream at that server, so this endpoint authenticates its caller with a shared secret
/// instead — otherwise anyone knowing the URL could forge vehicle state.
/// </remarks>
public sealed class TelemetryIngest
{
    private readonly ITelemetryDecoder _decoder;
    private readonly IVehicleStateRepository _repository;
    private readonly ChargingOptions _chargingOptions;
    private readonly IngestOptions _ingestOptions;
    private readonly TimeProvider _timeProvider;
    private readonly ILogger<TelemetryIngest> _logger;

    public TelemetryIngest(
        ITelemetryDecoder decoder,
        IVehicleStateRepository repository,
        IOptions<ChargingOptions> chargingOptions,
        IOptions<IngestOptions> ingestOptions,
        TimeProvider timeProvider,
        ILogger<TelemetryIngest> logger)
    {
        _decoder = decoder;
        _repository = repository;
        _chargingOptions = chargingOptions.Value;
        _ingestOptions = ingestOptions.Value;
        _timeProvider = timeProvider;
        _logger = logger;
    }

    [Function("TelemetryIngest")]
    public async Task<IActionResult> RunAsync(
        [HttpTrigger(AuthorizationLevel.Function, "post", Route = "telemetry")] HttpRequest request,
        CancellationToken cancellationToken)
    {
        if (!IsCallerAuthorized(request))
        {
            _logger.LogWarning("Rejected telemetry POST with missing or invalid shared secret.");
            return new StatusCodeResult((int)HttpStatusCode.Unauthorized);
        }

        byte[] body;
        using (var buffer = new MemoryStream())
        {
            await request.Body.CopyToAsync(buffer, cancellationToken);
            body = buffer.ToArray();
        }

        if (body.Length == 0)
        {
            return new BadRequestObjectResult("Empty payload.");
        }

        var now = _timeProvider.GetUtcNow();
        var observation = _decoder.Decode(body, now);
        if (observation is null)
        {
            // Ack anyway: returning an error would make the bridge retry a payload that will never
            // parse. The decoder has already logged the reason.
            return new OkObjectResult(new { status = "ignored" });
        }

        var updated = await _repository.MutateAsync(
            observation.Vin,
            state => OverrideEvaluator.Apply(state, observation, _chargingOptions),
            now,
            cancellationToken);

        _logger.LogInformation(
            "Telemetry {Vin}: state={ChargingState} amps={ChargeAmps} lastSet={LastSetAmps} override={OverrideActive}",
            observation.Vin,
            updated.ChargingState,
            updated.ChargeAmps,
            updated.LastSetAmps,
            updated.OverrideActive);

        return new OkObjectResult(new { status = "ok", vin = observation.Vin });
    }

    private bool IsCallerAuthorized(HttpRequest request)
    {
        if (string.IsNullOrWhiteSpace(_ingestOptions.SharedSecret))
        {
            // Fail closed. An unset secret in production would leave the endpoint open to spoofed
            // vehicle state, which drives real charging decisions.
            _logger.LogError("Ingest shared secret is not configured; refusing all telemetry.");
            return false;
        }

        if (!request.Headers.TryGetValue(IngestOptions.SharedSecretHeaderName, out var provided))
        {
            return false;
        }

        return FixedTimeEquals(provided.ToString(), _ingestOptions.SharedSecret);
    }

    private static bool FixedTimeEquals(string a, string b)
    {
        var left = Encoding.UTF8.GetBytes(a);
        var right = Encoding.UTF8.GetBytes(b);
        return CryptographicOperations.FixedTimeEquals(left, right);
    }
}
