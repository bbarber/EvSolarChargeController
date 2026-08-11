using System.Net.Http.Headers;
using System.Text.Json;
using System.Text.Json.Serialization;
using EvSolarChargeController.Functions.Configuration;
using EvSolarChargeController.Functions.Storage;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;

namespace EvSolarChargeController.Functions.Tesla;

public sealed record CommandResult(bool Success, string? Error)
{
    public static CommandResult Ok() => new(true, null);

    public static CommandResult Fail(string error) => new(false, error);
}

public interface ITeslaFleetClient
{
    /// <summary>
    /// Sets the charge current on an already-charging vehicle.
    /// </summary>
    /// <remarks>
    /// Only ever call this when telemetry says the car is actively charging. Command endpoints can
    /// wake a sleeping vehicle, which the design explicitly forbids.
    /// </remarks>
    Task<CommandResult> SetChargingAmpsAsync(string vin, int amps, CancellationToken cancellationToken = default);

    /// <summary>
    /// Stops an in-progress charge session. Used when the state-of-charge cap is reached, so this
    /// controller never charges past it regardless of the limit configured on the vehicle.
    /// </summary>
    Task<CommandResult> StopChargingAsync(string vin, CancellationToken cancellationToken = default);
}

/// <summary>
/// Tesla Fleet API client.
/// </summary>
/// <remarks>
/// <para>
/// Vehicles built after 2021 (everything except pre-2021 Model S/X and business fleet vehicles)
/// reject unsigned commands. Signing requires the app's private key and Tesla's vehicle-command
/// protocol, which is why <see cref="TeslaCommandMode.Proxy"/> is the default: commands are POSTed
/// to a tesla-http-proxy instance that signs them and forwards to Fleet API. The request/response
/// shape is identical either way, so only the base address changes.
/// </para>
/// </remarks>
public sealed class TeslaFleetClient : ITeslaFleetClient
{
    private static readonly JsonSerializerOptions JsonOptions = new(JsonSerializerDefaults.Web);

    private readonly HttpClient _http;
    private readonly TeslaOptions _options;
    private readonly SecretStoreOptions _secretOptions;
    private readonly IRefreshTokenStore _tokenStore;
    private readonly TimeProvider _timeProvider;
    private readonly ILogger<TeslaFleetClient> _logger;

    private string? _accessToken;
    private DateTimeOffset _accessTokenExpiresAt = DateTimeOffset.MinValue;

    public TeslaFleetClient(
        HttpClient http,
        IOptions<TeslaOptions> options,
        IOptions<SecretStoreOptions> secretOptions,
        IRefreshTokenStore tokenStore,
        TimeProvider timeProvider,
        ILogger<TeslaFleetClient> logger)
    {
        _http = http;
        _options = options.Value;
        _secretOptions = secretOptions.Value;
        _tokenStore = tokenStore;
        _timeProvider = timeProvider;
        _logger = logger;
    }

    public Task<CommandResult> SetChargingAmpsAsync(string vin, int amps, CancellationToken cancellationToken = default) =>
        SendCommandAsync(vin, "set_charging_amps", new { charging_amps = amps }, $"set_charging_amps={amps}", cancellationToken);

    public Task<CommandResult> StopChargingAsync(string vin, CancellationToken cancellationToken = default) =>
        SendCommandAsync(vin, "charge_stop", new { }, "charge_stop", cancellationToken);

    private async Task<CommandResult> SendCommandAsync(
        string vin,
        string command,
        object body,
        string description,
        CancellationToken cancellationToken)
    {
        string accessToken;
        try
        {
            accessToken = await GetAccessTokenAsync(cancellationToken);
        }
        catch (TeslaAuthException ex)
        {
            _logger.LogError(ex, "Tesla authentication failed; cannot send {Description}.", description);
            return CommandResult.Fail(ex.Message);
        }

        var baseUrl = ResolveCommandBaseUrl();
        if (baseUrl is null)
        {
            return CommandResult.Fail(
                "Tesla CommandMode is Proxy but CommandProxyBaseUrl is not configured. See docs/SETUP.md.");
        }

        var url = $"{baseUrl.TrimEnd('/')}/api/1/vehicles/{Uri.EscapeDataString(vin)}/command/{command}";

        using var request = new HttpRequestMessage(HttpMethod.Post, url);
        request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", accessToken);
        request.Content = JsonContent(body);

        if (_options.CommandMode == TeslaCommandMode.Proxy && !string.IsNullOrWhiteSpace(_options.CommandProxySharedSecret))
        {
            request.Headers.TryAddWithoutValidation("X-Proxy-Secret", _options.CommandProxySharedSecret);
        }

        try
        {
            using var response = await _http.SendAsync(request, cancellationToken);
            var responseBody = await response.Content.ReadAsStringAsync(cancellationToken);

            if (!response.IsSuccessStatusCode)
            {
                return CommandResult.Fail($"HTTP {(int)response.StatusCode} from {command}: {Truncate(responseBody)}");
            }

            // Fleet API wraps command outcomes in {"response":{"result":bool,"reason":string}} —
            // a 200 with result=false is still a failure.
            var parsed = TryParse<CommandEnvelope>(responseBody);
            if (parsed?.Response is { Result: false } failure)
            {
                var reason = failure.Reason ?? "no reason given";

                // Stopping a session that is not running is the desired end state, not an error.
                if (command == "charge_stop" && reason.Contains("not_charging", StringComparison.OrdinalIgnoreCase))
                {
                    _logger.LogInformation("{Vin} was already not charging; treating charge_stop as satisfied.", vin);
                    return CommandResult.Ok();
                }

                return CommandResult.Fail($"Vehicle rejected {command}: {reason}");
            }

            _logger.LogInformation("{Description} accepted for {Vin}.", description, vin);
            return CommandResult.Ok();
        }
        catch (Exception ex) when (ex is HttpRequestException or TaskCanceledException)
        {
            _logger.LogWarning(ex, "Transport failure sending {Command} to {Vin}.", command, vin);
            return CommandResult.Fail(ex.Message);
        }
    }

    private string? ResolveCommandBaseUrl() => _options.CommandMode switch
    {
        TeslaCommandMode.Direct => _options.FleetApiBaseUrl,
        TeslaCommandMode.Proxy => string.IsNullOrWhiteSpace(_options.CommandProxyBaseUrl)
            ? null
            : _options.CommandProxyBaseUrl,
        _ => null,
    };

    private async Task<string> GetAccessTokenAsync(CancellationToken cancellationToken)
    {
        var now = _timeProvider.GetUtcNow();
        if (_accessToken is not null && now < _accessTokenExpiresAt - _options.TokenRefreshSkew)
        {
            return _accessToken;
        }

        var refreshToken = await _tokenStore.GetAsync(_secretOptions.TeslaRefreshTokenSecretName, cancellationToken)
            ?? throw new TeslaAuthException(
                $"No Tesla refresh token available. Seed '{_secretOptions.TeslaRefreshTokenSecretName}' — see docs/SETUP.md.");

        using var request = new HttpRequestMessage(HttpMethod.Post, _options.TokenUrl)
        {
            Content = new FormUrlEncodedContent(new Dictionary<string, string>
            {
                ["grant_type"] = "refresh_token",
                ["client_id"] = _options.ClientId,
                ["refresh_token"] = refreshToken,
            }),
        };

        using var response = await _http.SendAsync(request, cancellationToken);
        var body = await response.Content.ReadAsStringAsync(cancellationToken);

        if (!response.IsSuccessStatusCode)
        {
            throw new TeslaAuthException($"Tesla token refresh returned HTTP {(int)response.StatusCode}: {Truncate(body)}");
        }

        var token = TryParse<TokenResponse>(body)
            ?? throw new TeslaAuthException("Tesla token endpoint returned an unreadable body.");

        if (string.IsNullOrWhiteSpace(token.AccessToken))
        {
            throw new TeslaAuthException("Tesla token endpoint returned no access_token.");
        }

        _accessToken = token.AccessToken;
        _accessTokenExpiresAt = now.AddSeconds(token.ExpiresIn > 0 ? token.ExpiresIn : 28_800);

        if (!string.IsNullOrWhiteSpace(token.RefreshToken) && token.RefreshToken != refreshToken)
        {
            await _tokenStore.SetAsync(_secretOptions.TeslaRefreshTokenSecretName, token.RefreshToken, cancellationToken);
        }

        return _accessToken;
    }

    private static HttpContent JsonContent(object value) =>
        new StringContent(JsonSerializer.Serialize(value, JsonOptions), System.Text.Encoding.UTF8, "application/json");

    private static T? TryParse<T>(string body)
    {
        try
        {
            return JsonSerializer.Deserialize<T>(body, JsonOptions);
        }
        catch (JsonException)
        {
            return default;
        }
    }

    private static string Truncate(string value, int max = 500) =>
        value.Length <= max ? value : value[..max] + "…";

    private sealed record CommandEnvelope
    {
        [JsonPropertyName("response")]
        public CommandResponse? Response { get; init; }
    }

    private sealed record CommandResponse
    {
        [JsonPropertyName("result")]
        public bool Result { get; init; }

        [JsonPropertyName("reason")]
        public string? Reason { get; init; }
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

public sealed class TeslaAuthException : Exception
{
    public TeslaAuthException(string message) : base(message)
    {
    }
}
