using EvSolarChargeController.Functions.Tesla;
using FluentAssertions;
using Google.Protobuf;
using Google.Protobuf.WellKnownTypes;
using Microsoft.Extensions.Logging.Abstractions;
using Telemetry.VehicleData;
using Xunit;
using DomainChargingState = EvSolarChargeController.Functions.Domain.ChargingState;

// Both the telemetry schema and protobuf's well-known types define Field and Value.
using Field = Telemetry.VehicleData.Field;
using Value = Telemetry.VehicleData.Value;

namespace EvSolarChargeController.Tests;

public class TelemetryDecoderTests
{
    private const string Vin = "5YJ3E1EA7KF000001";

    private static readonly DateTimeOffset Received = new(2026, 8, 11, 14, 0, 0, TimeSpan.Zero);

    private static readonly TelemetryDecoder Decoder = new(NullLogger<TelemetryDecoder>.Instance);

    private static byte[] Encode(params Datum[] data)
    {
        var payload = new Payload
        {
            Vin = Vin,
            CreatedAt = Timestamp.FromDateTimeOffset(Received),
        };
        payload.Data.AddRange(data);
        return payload.ToByteArray();
    }

    private static Datum Datum(Field key, Value value) => new() { Key = key, Value = value };

    [Fact]
    public void Decodes_charge_amps_and_detailed_charge_state()
    {
        var bytes = Encode(
            Datum(Field.ChargeAmps, new Value { IntValue = 12 }),
            Datum(Field.DetailedChargeState, new Value
            {
                DetailedChargeStateValue = DetailedChargeStateValue.DetailedChargeStateCharging,
            }));

        var observation = Decoder.Decode(bytes, Received);

        observation.Should().NotBeNull();
        observation!.Vin.Should().Be(Vin);
        observation.ReportedAmps.Should().Be(12);
        observation.ChargingState.Should().Be(DomainChargingState.Charging);
        observation.ObservedAt.Should().Be(Received);
    }

    [Theory]
    [InlineData(DetailedChargeStateValue.DetailedChargeStateDisconnected, DomainChargingState.Disconnected)]
    [InlineData(DetailedChargeStateValue.DetailedChargeStateCharging, DomainChargingState.Charging)]
    [InlineData(DetailedChargeStateValue.DetailedChargeStateComplete, DomainChargingState.Complete)]
    [InlineData(DetailedChargeStateValue.DetailedChargeStateStopped, DomainChargingState.Stopped)]
    [InlineData(DetailedChargeStateValue.DetailedChargeStateNoPower, DomainChargingState.NoPower)]
    [InlineData(DetailedChargeStateValue.DetailedChargeStateStarting, DomainChargingState.Starting)]
    public void Maps_every_detailed_charge_state(DetailedChargeStateValue proto, DomainChargingState expected)
    {
        var bytes = Encode(Datum(Field.DetailedChargeState, new Value { DetailedChargeStateValue = proto }));

        Decoder.Decode(bytes, Received)!.ChargingState.Should().Be(expected);
    }

    [Fact]
    public void Accepts_amps_sent_as_a_float()
    {
        var bytes = Encode(Datum(Field.ChargeAmps, new Value { FloatValue = 11.6f }));

        Decoder.Decode(bytes, Received)!.ReportedAmps.Should().Be(12);
    }

    [Fact]
    public void Accepts_amps_sent_as_a_string()
    {
        // Older firmware ships numeric signals as strings.
        var bytes = Encode(Datum(Field.ChargeAmps, new Value { StringValue = "14" }));

        Decoder.Decode(bytes, Received)!.ReportedAmps.Should().Be(14);
    }

    [Fact]
    public void Accepts_charge_state_sent_as_a_string()
    {
        var bytes = Encode(Datum(Field.DetailedChargeState, new Value { StringValue = "DetailedChargeStateCharging" }));

        Decoder.Decode(bytes, Received)!.ChargingState.Should().Be(DomainChargingState.Charging);
    }

    [Fact]
    public void Falls_back_to_charge_current_request_when_charge_amps_is_absent()
    {
        var bytes = Encode(Datum(Field.ChargeCurrentRequest, new Value { IntValue = 9 }));

        Decoder.Decode(bytes, Received)!.ReportedAmps.Should().Be(9);
    }

    [Fact]
    public void Prefers_charge_amps_over_charge_current_request()
    {
        var bytes = Encode(
            Datum(Field.ChargeAmps, new Value { IntValue = 12 }),
            Datum(Field.ChargeCurrentRequest, new Value { IntValue = 9 }));

        Decoder.Decode(bytes, Received)!.ReportedAmps.Should().Be(12);
    }

    [Fact]
    public void Reads_the_vehicle_reported_maximum()
    {
        var bytes = Encode(Datum(Field.ChargeCurrentRequestMax, new Value { IntValue = 16 }));

        Decoder.Decode(bytes, Received)!.ReportedMaxAmps.Should().Be(16);
    }

    [Fact]
    public void Reads_a_disengaged_charge_port_latch()
    {
        var bytes = Encode(Datum(Field.ChargePortLatch, new Value
        {
            ChargePortLatchValue = ChargePortLatchValue.ChargePortLatchDisengaged,
        }));

        Decoder.Decode(bytes, Received)!.LatchDisengaged.Should().BeTrue();
    }

    [Fact]
    public void Reads_an_engaged_charge_port_latch()
    {
        var bytes = Encode(Datum(Field.ChargePortLatch, new Value
        {
            ChargePortLatchValue = ChargePortLatchValue.ChargePortLatchEngaged,
        }));

        Decoder.Decode(bytes, Received)!.LatchDisengaged.Should().BeFalse();
    }

    [Fact]
    public void Ignores_fields_this_controller_does_not_use()
    {
        var bytes = Encode(
            Datum(Field.VehicleSpeed, new Value { FloatValue = 55f }),
            Datum(Field.ChargeAmps, new Value { IntValue = 10 }));

        var observation = Decoder.Decode(bytes, Received);

        observation!.ReportedAmps.Should().Be(10);
    }

    [Fact]
    public void Skips_values_explicitly_marked_invalid()
    {
        var bytes = Encode(Datum(Field.ChargeAmps, new Value { Invalid = true }));

        Decoder.Decode(bytes, Received)!.ReportedAmps.Should().BeNull();
    }

    [Fact]
    public void Returns_null_for_a_payload_with_no_vin()
    {
        var payload = new Payload { CreatedAt = Timestamp.FromDateTimeOffset(Received) };

        Decoder.Decode(payload.ToByteArray(), Received).Should().BeNull();
    }

    [Fact]
    public void Returns_null_for_bytes_that_are_not_a_payload()
    {
        var garbage = new byte[] { 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF };

        Decoder.Decode(garbage, Received).Should().BeNull();
    }

    [Fact]
    public void Falls_back_to_the_receive_time_when_the_payload_has_no_timestamp()
    {
        var payload = new Payload { Vin = Vin };
        payload.Data.Add(Datum(Field.ChargeAmps, new Value { IntValue = 10 }));

        Decoder.Decode(payload.ToByteArray(), Received)!.ObservedAt.Should().Be(Received);
    }
}
