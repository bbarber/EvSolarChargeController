namespace EvSolarChargeController.Functions.Domain;

/// <summary>
/// Normalized charge state, mapped from Tesla's <c>DetailedChargeState</c> telemetry field.
/// </summary>
/// <remarks>
/// The proto also defines a <c>ChargingState</c> enum, but it is explicitly marked
/// "deprecated and not used" in Tesla's schema, so <c>DetailedChargeState</c> (field 179)
/// is the authoritative source.
/// </remarks>
public enum ChargingState
{
    Unknown = 0,
    Disconnected = 1,
    NoPower = 2,
    Starting = 3,
    Charging = 4,
    Complete = 5,
    Stopped = 6,
}

public static class ChargingStateExtensions
{
    /// <summary>True when the car is actively drawing current and will honour an amp change.</summary>
    public static bool IsActivelyCharging(this ChargingState state) => state == ChargingState.Charging;

    /// <summary>
    /// True when the connector is out of the car. This is the event that clears a manual override —
    /// per the spec, an override persists until the vehicle unplugs.
    /// </summary>
    public static bool IsUnplugged(this ChargingState state) => state == ChargingState.Disconnected;

    /// <summary>
    /// True when the cable is still attached, even if not drawing power. Used together with the
    /// charge-port latch to decide whether an override should survive.
    /// </summary>
    public static bool IsPluggedIn(this ChargingState state) => state is ChargingState.Charging
        or ChargingState.Complete
        or ChargingState.Starting
        or ChargingState.Stopped
        or ChargingState.NoPower;
}
