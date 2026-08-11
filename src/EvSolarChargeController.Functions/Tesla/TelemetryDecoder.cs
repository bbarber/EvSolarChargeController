using System.Globalization;
using EvSolarChargeController.Functions.Domain;
using Google.Protobuf;
using Microsoft.Extensions.Logging;
using Telemetry.VehicleData;

// The proto defines its own ChargingState (marked deprecated by Tesla) which collides with the
// domain enum. Alias the domain one so every bare mention here is unambiguous.
using ChargingState = EvSolarChargeController.Functions.Domain.ChargingState;

namespace EvSolarChargeController.Functions.Tesla;

public interface ITelemetryDecoder
{
    /// <summary>
    /// Decodes a Tesla Fleet Telemetry <c>Payload</c> and reduces it to the charge-related fields
    /// this controller cares about. Returns null when the payload cannot be parsed.
    /// </summary>
    TelemetryObservation? Decode(ReadOnlyMemory<byte> payload, DateTimeOffset receivedAt);
}

/// <summary>
/// Maps Tesla's telemetry protobuf onto <see cref="TelemetryObservation"/>.
/// </summary>
/// <remarks>
/// Signals are streamed on change, so any given payload carries only the fields that moved. Every
/// extracted value is therefore nullable and the caller merges it into stored state rather than
/// replacing it.
/// </remarks>
public sealed class TelemetryDecoder : ITelemetryDecoder
{
    private readonly ILogger<TelemetryDecoder> _logger;

    public TelemetryDecoder(ILogger<TelemetryDecoder> logger)
    {
        _logger = logger;
    }

    public TelemetryObservation? Decode(ReadOnlyMemory<byte> payload, DateTimeOffset receivedAt)
    {
        Payload parsed;
        try
        {
            parsed = Payload.Parser.ParseFrom(payload.Span);
        }
        catch (InvalidProtocolBufferException ex)
        {
            _logger.LogWarning(ex, "Discarding telemetry payload that is not a valid Fleet Telemetry Payload message.");
            return null;
        }

        if (string.IsNullOrWhiteSpace(parsed.Vin))
        {
            _logger.LogWarning("Discarding telemetry payload with no VIN.");
            return null;
        }

        int? chargeAmps = null;
        int? chargeCurrentRequest = null;
        int? maxAmps = null;
        int? soc = null;
        int? batteryLevel = null;
        ChargingState? chargingState = null;
        bool? latchDisengaged = null;

        foreach (var datum in parsed.Data)
        {
            if (datum.Value is null || datum.Value.ValueCase == Value.ValueOneofCase.Invalid)
            {
                continue;
            }

            switch (datum.Key)
            {
                case Field.ChargeAmps:
                    chargeAmps = AsInt(datum.Value);
                    break;

                case Field.ChargeCurrentRequest:
                    chargeCurrentRequest = AsInt(datum.Value);
                    break;

                case Field.ChargeCurrentRequestMax:
                    maxAmps = AsInt(datum.Value);
                    break;

                case Field.DetailedChargeState:
                    chargingState = AsChargingState(datum.Value);
                    break;

                case Field.ChargePortLatch:
                    latchDisengaged = AsLatchDisengaged(datum.Value);
                    break;

                case Field.BatteryLevel:
                    batteryLevel = AsInt(datum.Value);
                    break;

                case Field.Soc:
                    soc = AsInt(datum.Value);
                    break;

                default:
                    break;
            }
        }

        var observedAt = parsed.CreatedAt is not null
            ? parsed.CreatedAt.ToDateTimeOffset()
            : receivedAt;

        return new TelemetryObservation
        {
            Vin = parsed.Vin,
            ObservedAt = observedAt,
            // ChargeAmps is the vehicle's configured charge current. Some firmware reports it only
            // via ChargeCurrentRequest, so fall back to that before giving up.
            ReportedAmps = chargeAmps ?? chargeCurrentRequest,
            ReportedMaxAmps = maxAmps,
            // BatteryLevel is the dashboard percentage; Soc is the underlying figure and is not
            // always present. Prefer the former and fall back, so the SoC cap still has a value
            // to work from on firmware that only sends one of them.
            BatteryLevelPercent = batteryLevel ?? soc,
            ChargingState = chargingState,
            LatchDisengaged = latchDisengaged,
        };
    }

    /// <summary>
    /// Coerces a telemetry value to an integer. Tesla has shipped these as ints, floats and
    /// strings across firmware versions, so accept whichever arrives.
    /// </summary>
    private static int? AsInt(Value value) => value.ValueCase switch
    {
        Value.ValueOneofCase.IntValue => value.IntValue,
        Value.ValueOneofCase.LongValue => (int)value.LongValue,
        Value.ValueOneofCase.FloatValue => (int)Math.Round(value.FloatValue, MidpointRounding.AwayFromZero),
        Value.ValueOneofCase.DoubleValue => (int)Math.Round(value.DoubleValue, MidpointRounding.AwayFromZero),
        Value.ValueOneofCase.StringValue => double.TryParse(
            value.StringValue,
            NumberStyles.Float,
            CultureInfo.InvariantCulture,
            out var parsed)
                ? (int)Math.Round(parsed, MidpointRounding.AwayFromZero)
                : null,
        _ => null,
    };

    private static ChargingState? AsChargingState(Value value)
    {
        if (value.ValueCase == Value.ValueOneofCase.DetailedChargeStateValue)
        {
            return value.DetailedChargeStateValue switch
            {
                Telemetry.VehicleData.DetailedChargeStateValue.DetailedChargeStateDisconnected => ChargingState.Disconnected,
                Telemetry.VehicleData.DetailedChargeStateValue.DetailedChargeStateNoPower => ChargingState.NoPower,
                Telemetry.VehicleData.DetailedChargeStateValue.DetailedChargeStateStarting => ChargingState.Starting,
                Telemetry.VehicleData.DetailedChargeStateValue.DetailedChargeStateCharging => ChargingState.Charging,
                Telemetry.VehicleData.DetailedChargeStateValue.DetailedChargeStateComplete => ChargingState.Complete,
                Telemetry.VehicleData.DetailedChargeStateValue.DetailedChargeStateStopped => ChargingState.Stopped,
                _ => ChargingState.Unknown,
            };
        }

        // Older firmware sends the state as a bare string such as "Charging" or
        // "DetailedChargeStateCharging".
        if (value.ValueCase == Value.ValueOneofCase.StringValue)
        {
            var text = value.StringValue;
            const string prefix = "DetailedChargeState";
            if (text.StartsWith(prefix, StringComparison.OrdinalIgnoreCase))
            {
                text = text[prefix.Length..];
            }

            if (Enum.TryParse<ChargingState>(text, ignoreCase: true, out var parsed))
            {
                return parsed;
            }
        }

        return null;
    }

    private static bool? AsLatchDisengaged(Value value) => value.ValueCase switch
    {
        Value.ValueOneofCase.ChargePortLatchValue => value.ChargePortLatchValue switch
        {
            Telemetry.VehicleData.ChargePortLatchValue.ChargePortLatchDisengaged => true,
            Telemetry.VehicleData.ChargePortLatchValue.ChargePortLatchEngaged => false,
            Telemetry.VehicleData.ChargePortLatchValue.ChargePortLatchBlocking => false,
            _ => null,
        },
        Value.ValueOneofCase.StringValue =>
            value.StringValue.Contains("Disengaged", StringComparison.OrdinalIgnoreCase) ? true
            : value.StringValue.Contains("Engaged", StringComparison.OrdinalIgnoreCase) ? false
            : null,
        _ => null,
    };
}
