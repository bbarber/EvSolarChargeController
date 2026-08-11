using EvSolarChargeController.Functions.Configuration;
using EvSolarChargeController.Functions.Domain;
using FluentAssertions;
using Xunit;

namespace EvSolarChargeController.Tests;

public class PollingWindowTests
{
    private static PollingWindow Window() => new(new PollingWindowOptions
    {
        TimeZone = "America/Chicago",
        StartHourLocal = 9,
        EndHourLocal = 19,
    });

    [Fact]
    public void Open_during_the_middle_of_a_summer_day()
    {
        // 2026-08-11 14:00 CDT = 19:00 UTC
        Window().IsOpen(new DateTimeOffset(2026, 8, 11, 19, 0, 0, TimeSpan.Zero)).Should().BeTrue();
    }

    [Fact]
    public void Open_at_the_first_fire_of_the_day()
    {
        // 09:00 CDT = 14:00 UTC
        Window().IsOpen(new DateTimeOffset(2026, 8, 11, 14, 0, 0, TimeSpan.Zero)).Should().BeTrue();
    }

    [Fact]
    public void Open_at_the_last_fire_of_the_day()
    {
        // 18:40 CDT = 23:40 UTC — the final :40 fire, still before the 19:00 cutoff.
        Window().IsOpen(new DateTimeOffset(2026, 8, 11, 23, 40, 0, TimeSpan.Zero)).Should().BeTrue();
    }

    [Fact]
    public void Closed_at_the_end_boundary()
    {
        // 19:00 CDT = 00:00 UTC next day.
        Window().IsOpen(new DateTimeOffset(2026, 8, 12, 0, 0, 0, TimeSpan.Zero)).Should().BeFalse();
    }

    [Fact]
    public void Closed_before_the_start_boundary()
    {
        // 08:59 CDT = 13:59 UTC
        Window().IsOpen(new DateTimeOffset(2026, 8, 11, 13, 59, 0, TimeSpan.Zero)).Should().BeFalse();
    }

    [Fact]
    public void Closed_overnight()
    {
        // 03:00 CDT = 08:00 UTC
        Window().IsOpen(new DateTimeOffset(2026, 8, 11, 8, 0, 0, TimeSpan.Zero)).Should().BeFalse();
    }

    [Fact]
    public void Tracks_the_window_across_the_dst_boundary()
    {
        // In January, Chicago is CST (UTC-6), so 09:00 local = 15:00 UTC — an hour later in UTC
        // than the same local time in summer. A UTC-only check would poll at the wrong hours.
        var window = Window();

        window.IsOpen(new DateTimeOffset(2026, 1, 15, 14, 0, 0, TimeSpan.Zero)).Should().BeFalse(); // 08:00 CST
        window.IsOpen(new DateTimeOffset(2026, 1, 15, 15, 0, 0, TimeSpan.Zero)).Should().BeTrue();  // 09:00 CST
    }

    [Fact]
    public void Resolves_the_iana_zone_on_this_platform()
    {
        Window().TimeZoneId.Should().NotBeNullOrWhiteSpace();
    }
}
