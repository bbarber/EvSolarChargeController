namespace EvSolarChargeController.TelemetryBridge;

public sealed class BridgeOptions
{
    public const string SectionName = "BRIDGE";

    /// <summary>
    /// ZeroMQ endpoint fleet-telemetry publishes on. Both containers share a network namespace
    /// inside the Container App, so records never leave localhost.
    /// </summary>
    public string ZmqEndpoint { get; set; } = "tcp://127.0.0.1:5284";

    /// <summary>
    /// Topic prefix to subscribe to. fleet-telemetry builds topics as "&lt;namespace&gt;_&lt;txType&gt;";
    /// txType "V" carries vehicle data. Empty subscribes to everything.
    /// </summary>
    public string TopicPrefix { get; set; } = string.Empty;

    /// <summary>Absolute URL of the TelemetryIngest function.</summary>
    public string IngestUrl { get; set; } = string.Empty;

    /// <summary>Shared secret presented to the Function, matching its Ingest:SharedSecret.</summary>
    public string IngestSharedSecret { get; set; } = string.Empty;

    /// <summary>Function-level auth key, when the ingest function is not anonymous.</summary>
    public string IngestFunctionKey { get; set; } = string.Empty;

    /// <summary>Only vehicle-data records matter; other record types are dropped without a POST.</summary>
    public string VehicleDataTopicSuffix { get; set; } = "_V";

    public int MaxRetries { get; set; } = 2;

    public TimeSpan RetryDelay { get; set; } = TimeSpan.FromSeconds(2);
}
