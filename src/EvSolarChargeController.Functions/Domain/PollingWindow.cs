using EvSolarChargeController.Functions.Configuration;

namespace EvSolarChargeController.Functions.Domain;

/// <summary>
/// Decides whether a given instant falls inside the daylight polling window.
/// </summary>
/// <remarks>
/// The NCRONTAB expression already restricts firing to daytime hours, but it interprets its hours
/// in whatever <c>WEBSITE_TIME_ZONE</c> says. If that app setting is ever lost the timer silently
/// reverts to UTC and would burn the Enphase monthly budget on overnight calls, so the window is
/// re-checked here against an explicitly resolved time zone.
/// </remarks>
public sealed class PollingWindow
{
    private readonly PollingWindowOptions _options;
    private readonly TimeZoneInfo _timeZone;

    public PollingWindow(PollingWindowOptions options)
    {
        ArgumentNullException.ThrowIfNull(options);
        _options = options;
        _timeZone = ResolveTimeZone(options.TimeZone);
    }

    public string TimeZoneId => _timeZone.Id;

    public DateTimeOffset ToLocal(DateTimeOffset instant) => TimeZoneInfo.ConvertTime(instant, _timeZone);

    /// <summary>True when <paramref name="instant"/> is within [StartHourLocal, EndHourLocal) local time.</summary>
    public bool IsOpen(DateTimeOffset instant)
    {
        var local = ToLocal(instant);
        return local.Hour >= _options.StartHourLocal && local.Hour < _options.EndHourLocal;
    }

    public string Describe(DateTimeOffset instant)
    {
        var local = ToLocal(instant);
        return $"{local:yyyy-MM-dd HH:mm} {_timeZone.Id} (window {_options.StartHourLocal:00}:00-{_options.EndHourLocal:00}:00)";
    }

    /// <summary>
    /// Resolves an IANA id such as America/Chicago. Windows ships Windows-style ids, so fall back
    /// to the mapped equivalent when the IANA lookup misses on a Windows-hosted Function.
    /// </summary>
    private static TimeZoneInfo ResolveTimeZone(string id)
    {
        try
        {
            return TimeZoneInfo.FindSystemTimeZoneById(id);
        }
        catch (TimeZoneNotFoundException)
        {
            if (TimeZoneInfo.TryConvertIanaIdToWindowsId(id, out var windowsId) && windowsId is not null)
            {
                return TimeZoneInfo.FindSystemTimeZoneById(windowsId);
            }

            throw;
        }
    }
}
