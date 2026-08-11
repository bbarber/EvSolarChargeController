using System.Globalization;
using System.Net;
using System.Net.Http.Headers;
using System.Text;
using System.Text.Json;
using System.Text.Json.Serialization;
using EvSolarChargeController.Functions.Configuration;
using EvSolarChargeController.Functions.Storage;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;

namespace EvSolarChargeController.Functions.Enphase;

/// <summary>A single production sample from the Enphase cloud.</summary>
public sealed record SolarProduction(double Watts, DateTimeOffset ReadingAt);

/// <summary>Why a poll produced no reading. Callers log and skip the cycle rather than retrying.</summary>
public enum EnphaseFailureReason
{
    None,
    RateLimited,
    QuotaGuard,
    AuthFailed,
    TransportError,
    UnexpectedResponse,
}

public sealed record EnphaseResult(SolarProduction? Production, EnphaseFailureReason Reason, string? Message)
{
    public bool Success => Production is not null;

    public static EnphaseResult Ok(SolarProduction production) => new(production, EnphaseFailureReason.None, null);

    public static EnphaseResult Fail(EnphaseFailureReason reason, string message) => new(null, reason, message);
}

public interface IEnphaseClient
{
    Task<EnphaseResult> GetCurrentProductionAsync(DateTimeOffset now, CancellationToken cancellationToken = default);
}

/// <summary>
/// Enphase Enlighten API v4 client for the free "Watt" plan.
/// </summary>
/// <remarks>
/// Deliberately does not retry: the Watt plan allows only 1000 calls a month, and a missed
/// 20-minute cycle is harmless because the next one picks up fresh data. Every call is counted
/// in Table Storage and refused once the configured monthly budget is exhausted.
/// </remarks>
public sealed class EnphaseClient : IEnphaseClient
{
    private static readonly JsonSerializerOptions JsonOptions = new(JsonSerializerDefaults.Web);

    private readonly HttpClient _http;
    private readonly EnphaseOptions _options;
    private readonly SecretStoreOptions _secretOptions;
    private readonly IRefreshTokenStore _tokenStore;
    private readonly IApiUsageRepository _usage;
    private readonly ILogger<EnphaseClient> _logger;

    // Access tokens live a day; cache in-process so a warm instance does not spend a token call per cycle.
    private string? _accessToken;
    private DateTimeOffset _accessTokenExpiresAt = DateTimeOffset.MinValue;

    public EnphaseClient(
        HttpClient http,
        IOptions<EnphaseOptions> options,
        IOptions<SecretStoreOptions> secretOptions,
        IRefreshTokenStore tokenStore,
        IApiUsageRepository usage,
        ILogger<EnphaseClient> logger)
    {
        _http = http;
        _options = options.Value;
        _secretOptions = secretOptions.Value;
        _tokenStore = tokenStore;
        _usage = usage;
        _logger = logger;
    }

    public async Task<EnphaseResult> GetCurrentProductionAsync(DateTimeOffset now, CancellationToken cancellationToken = default)
    {
        var used = await _usage.GetMonthlyCountAsync(ApiUsageRepository.EnphaseProvider, now, cancellationToken);
        if (used >= _options.MonthlyCallBudget)
        {
            var message = $"Enphase monthly call budget exhausted ({used}/{_options.MonthlyCallBudget}); skipping poll.";
            _logger.LogError("{Message}", message);
            return EnphaseResult.Fail(EnphaseFailureReason.QuotaGuard, message);
        }

        string accessToken;
        try
        {
            accessToken = await GetAccessTokenAsync(now, cancellationToken);
        }
        catch (EnphaseAuthException ex)
        {
            _logger.LogError(ex, "Enphase authentication failed.");
            return EnphaseResult.Fail(EnphaseFailureReason.AuthFailed, ex.Message);
        }

        var url = $"{_options.BaseUrl.TrimEnd('/')}/systems/{_options.SystemId}/summary?key={Uri.EscapeDataString(_options.ApiKey)}";

        using var request = new HttpRequestMessage(HttpMethod.Get, url);
        request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", accessToken);

        HttpResponseMessage response;
        try
        {
            response = await _http.SendAsync(request, cancellationToken);
        }
        catch (Exception ex) when (ex is HttpRequestException or TaskCanceledException)
        {
            await _usage.RecordCallAsync(ApiUsageRepository.EnphaseProvider, now, succeeded: false, cancellationToken);
            _logger.LogWarning(ex, "Enphase production request failed at the transport layer.");
            return EnphaseResult.Fail(EnphaseFailureReason.TransportError, ex.Message);
        }

        using (response)
        {
            var succeeded = response.IsSuccessStatusCode;
            var total = await _usage.RecordCallAsync(ApiUsageRepository.EnphaseProvider, now, succeeded, cancellationToken);

            _logger.LogInformation(
                "Enphase call {Total}/{Budget} this month -> HTTP {StatusCode}.",
                total,
                _options.MonthlyCallBudget,
                (int)response.StatusCode);

            if (response.StatusCode == HttpStatusCode.TooManyRequests)
            {
                return EnphaseResult.Fail(
                    EnphaseFailureReason.RateLimited,
                    "Enphase returned 429; skipping this cycle without retrying.");
            }

            if (!succeeded)
            {
                var body = await SafeReadAsync(response, cancellationToken);
                return EnphaseResult.Fail(
                    EnphaseFailureReason.UnexpectedResponse,
                    $"Enphase returned HTTP {(int)response.StatusCode}: {Truncate(body, 500)}");
            }

            var payload = await response.Content.ReadFromJsonSafeAsync<SystemSummaryResponse>(JsonOptions, cancellationToken);
            if (payload?.CurrentPower is not { } watts)
            {
                return EnphaseResult.Fail(
                    EnphaseFailureReason.UnexpectedResponse,
                    "Enphase summary response did not include current_power.");
            }

            var readingAt = payload.LastReportAt is { } epoch
                ? DateTimeOffset.FromUnixTimeSeconds(epoch)
                : now;

            return EnphaseResult.Ok(new SolarProduction(watts, readingAt));
        }
    }

    private async Task<string> GetAccessTokenAsync(DateTimeOffset now, CancellationToken cancellationToken)
    {
        if (_accessToken is not null && now < _accessTokenExpiresAt - _options.TokenRefreshSkew)
        {
            return _accessToken;
        }

        var refreshToken = await _tokenStore.GetAsync(_secretOptions.EnphaseRefreshTokenSecretName, cancellationToken)
            ?? throw new EnphaseAuthException(
                $"No Enphase refresh token available. Seed '{_secretOptions.EnphaseRefreshTokenSecretName}' in Key Vault — see docs/SETUP.md.");

        var url = $"{_options.TokenUrl}?grant_type=refresh_token&refresh_token={Uri.EscapeDataString(refreshToken)}";

        using var request = new HttpRequestMessage(HttpMethod.Post, url);
        var basic = Convert.ToBase64String(Encoding.UTF8.GetBytes($"{_options.ClientId}:{_options.ClientSecret}"));
        request.Headers.Authorization = new AuthenticationHeaderValue("Basic", basic);
        request.Content = new StringContent(string.Empty, Encoding.UTF8, "application/x-www-form-urlencoded");

        using var response = await _http.SendAsync(request, cancellationToken);
        if (!response.IsSuccessStatusCode)
        {
            var body = await SafeReadAsync(response, cancellationToken);
            throw new EnphaseAuthException(
                $"Enphase token refresh returned HTTP {(int)response.StatusCode}: {Truncate(body, 500)}. " +
                "Enphase refresh tokens expire after one month — a full re-authorization may be required.");
        }

        var token = await response.Content.ReadFromJsonSafeAsync<TokenResponse>(JsonOptions, cancellationToken)
            ?? throw new EnphaseAuthException("Enphase token endpoint returned an unreadable body.");

        if (string.IsNullOrWhiteSpace(token.AccessToken))
        {
            throw new EnphaseAuthException("Enphase token endpoint returned no access_token.");
        }

        _accessToken = token.AccessToken;
        _accessTokenExpiresAt = now.AddSeconds(token.ExpiresIn > 0 ? token.ExpiresIn : 86_400);

        // Enphase rotates the refresh token on every use; persist it or the next run is locked out.
        if (!string.IsNullOrWhiteSpace(token.RefreshToken) && token.RefreshToken != refreshToken)
        {
            await _tokenStore.SetAsync(_secretOptions.EnphaseRefreshTokenSecretName, token.RefreshToken, cancellationToken);
        }

        return _accessToken;
    }

    private static async Task<string> SafeReadAsync(HttpResponseMessage response, CancellationToken cancellationToken)
    {
        try
        {
            return await response.Content.ReadAsStringAsync(cancellationToken);
        }
        catch
        {
            return "<unreadable>";
        }
    }

    private static string Truncate(string value, int max) =>
        value.Length <= max ? value : value[..max] + "…";

    private sealed record SystemSummaryResponse
    {
        [JsonPropertyName("current_power")]
        public double? CurrentPower { get; init; }

        [JsonPropertyName("energy_today")]
        public double? EnergyToday { get; init; }

        [JsonPropertyName("last_report_at")]
        public long? LastReportAt { get; init; }

        [JsonPropertyName("status")]
        public string? Status { get; init; }
    }

    private sealed record TokenResponse
    {
        [JsonPropertyName("access_token")]
        public string? AccessToken { get; init; }

        [JsonPropertyName("refresh_token")]
        public string? RefreshToken { get; init; }

        [JsonPropertyName("expires_in")]
        public int ExpiresIn { get; init; }
    }
}

public sealed class EnphaseAuthException : Exception
{
    public EnphaseAuthException(string message) : base(message)
    {
    }
}

internal static class HttpContentJsonExtensions
{
    /// <summary>Reads JSON without throwing on malformed bodies — callers treat null as "skip this cycle".</summary>
    public static async Task<T?> ReadFromJsonSafeAsync<T>(
        this HttpContent content,
        JsonSerializerOptions options,
        CancellationToken cancellationToken)
    {
        try
        {
            await using var stream = await content.ReadAsStreamAsync(cancellationToken);
            return await JsonSerializer.DeserializeAsync<T>(stream, options, cancellationToken);
        }
        catch (JsonException)
        {
            return default;
        }
    }

    public static string ToInvariantString(this double value) => value.ToString(CultureInfo.InvariantCulture);
}
