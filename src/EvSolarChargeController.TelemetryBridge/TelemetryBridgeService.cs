using System.Net.Http.Headers;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;
using NetMQ;
using NetMQ.Sockets;

namespace EvSolarChargeController.TelemetryBridge;

/// <summary>
/// Forwards Tesla Fleet Telemetry records from fleet-telemetry's ZeroMQ dispatcher to the
/// TelemetryIngest Function.
/// </summary>
/// <remarks>
/// This exists because vehicles speak a mutual-TLS WebSocket that only Tesla's fleet-telemetry
/// server terminates, and that server can dispatch to kafka, kinesis, pubsub, zmq, redis or mqtt —
/// but not to an HTTP endpoint. ZeroMQ is the only one of those that needs no broker, so the
/// server publishes to a loopback socket and this process relays each record onward unmodified.
/// </remarks>
public sealed class TelemetryBridgeService : BackgroundService
{
    private readonly BridgeOptions _options;
    private readonly IHttpClientFactory _httpClientFactory;
    private readonly ILogger<TelemetryBridgeService> _logger;

    public TelemetryBridgeService(
        IOptions<BridgeOptions> options,
        IHttpClientFactory httpClientFactory,
        ILogger<TelemetryBridgeService> logger)
    {
        _options = options.Value;
        _httpClientFactory = httpClientFactory;
        _logger = logger;
    }

    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        if (string.IsNullOrWhiteSpace(_options.IngestUrl))
        {
            throw new InvalidOperationException("BRIDGE__IngestUrl is required.");
        }

        if (string.IsNullOrWhiteSpace(_options.IngestSharedSecret))
        {
            throw new InvalidOperationException(
                "BRIDGE__IngestSharedSecret is required; the Function rejects unauthenticated posts.");
        }

        _logger.LogInformation(
            "Bridge starting: subscribing to {Endpoint} (prefix '{Prefix}') -> {IngestUrl}",
            _options.ZmqEndpoint,
            _options.TopicPrefix,
            _options.IngestUrl);

        using var subscriber = new SubscriberSocket();
        subscriber.Options.ReceiveHighWatermark = 1000;
        subscriber.Connect(_options.ZmqEndpoint);
        subscriber.Subscribe(_options.TopicPrefix);

        var forwarded = 0L;
        var dropped = 0L;

        while (!stoppingToken.IsCancellationRequested)
        {
            // Poll rather than block forever so cancellation is honoured promptly on shutdown.
            if (!subscriber.TryReceiveFrameString(TimeSpan.FromMilliseconds(500), out var topic))
            {
                continue;
            }

            if (!subscriber.TryReceiveFrameBytes(TimeSpan.FromSeconds(1), out var payload) || payload is null)
            {
                _logger.LogWarning("Received topic '{Topic}' with no payload frame; dropping.", topic);
                continue;
            }

            if (!IsVehicleData(topic))
            {
                dropped++;
                continue;
            }

            if (await ForwardAsync(payload, stoppingToken))
            {
                forwarded++;
            }

            if ((forwarded + dropped) % 100 == 0)
            {
                _logger.LogInformation("Bridge totals: {Forwarded} forwarded, {Dropped} non-vehicle records dropped.", forwarded, dropped);
            }
        }

        _logger.LogInformation("Bridge stopping after forwarding {Forwarded} records.", forwarded);
    }

    private bool IsVehicleData(string? topic) =>
        topic is not null && topic.EndsWith(_options.VehicleDataTopicSuffix, StringComparison.Ordinal);

    private async Task<bool> ForwardAsync(byte[] payload, CancellationToken cancellationToken)
    {
        var client = _httpClientFactory.CreateClient("ingest");

        for (var attempt = 0; attempt <= _options.MaxRetries; attempt++)
        {
            try
            {
                using var content = new ByteArrayContent(payload);
                content.Headers.ContentType = new MediaTypeHeaderValue("application/x-protobuf");

                using var request = new HttpRequestMessage(HttpMethod.Post, _options.IngestUrl)
                {
                    Content = content,
                };
                request.Headers.TryAddWithoutValidation("X-Ingest-Secret", _options.IngestSharedSecret);

                if (!string.IsNullOrWhiteSpace(_options.IngestFunctionKey))
                {
                    request.Headers.TryAddWithoutValidation("x-functions-key", _options.IngestFunctionKey);
                }

                using var response = await client.SendAsync(request, cancellationToken);

                if (response.IsSuccessStatusCode)
                {
                    return true;
                }

                // A rejected payload will be rejected again; only transient failures are worth a retry.
                if ((int)response.StatusCode is >= 400 and < 500)
                {
                    _logger.LogError(
                        "Ingest rejected a record with HTTP {StatusCode}; not retrying.",
                        (int)response.StatusCode);
                    return false;
                }

                _logger.LogWarning(
                    "Ingest returned HTTP {StatusCode} (attempt {Attempt}/{Max}).",
                    (int)response.StatusCode,
                    attempt + 1,
                    _options.MaxRetries + 1);
            }
            catch (OperationCanceledException) when (cancellationToken.IsCancellationRequested)
            {
                throw;
            }
            catch (Exception ex) when (ex is HttpRequestException or TaskCanceledException)
            {
                _logger.LogWarning(
                    ex,
                    "Transport failure posting to ingest (attempt {Attempt}/{Max}).",
                    attempt + 1,
                    _options.MaxRetries + 1);
            }

            if (attempt < _options.MaxRetries)
            {
                await Task.Delay(_options.RetryDelay, cancellationToken);
            }
        }

        // Telemetry is a continuous stream; the next payload supersedes this one, so dropping it
        // is preferable to blocking the subscriber and building an unbounded backlog.
        _logger.LogError("Giving up on a telemetry record after {Attempts} attempts.", _options.MaxRetries + 1);
        return false;
    }
}
