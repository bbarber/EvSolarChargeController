using Azure;
using Azure.Data.Tables;
using Azure.Security.KeyVault.Secrets;
using EvSolarChargeController.Functions.Configuration;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Options;

namespace EvSolarChargeController.Functions.Storage;

/// <summary>
/// Read/write access to rotating OAuth refresh tokens.
/// </summary>
/// <remarks>
/// Both providers hand back a new refresh token on every refresh, and Enphase's expires after a
/// month, so the token cannot live in an immutable app setting — it has to be written back
/// somewhere durable or the integration silently dies the first time the old one is rejected.
/// </remarks>
public interface IRefreshTokenStore
{
    Task<string?> GetAsync(string name, CancellationToken cancellationToken = default);

    Task SetAsync(string name, string value, CancellationToken cancellationToken = default);
}

/// <summary>Key Vault backed store. Each write creates a new secret version, giving a rollback trail.</summary>
public sealed class KeyVaultRefreshTokenStore : IRefreshTokenStore
{
    private readonly SecretClient _client;
    private readonly ILogger<KeyVaultRefreshTokenStore> _logger;

    public KeyVaultRefreshTokenStore(SecretClient client, ILogger<KeyVaultRefreshTokenStore> logger)
    {
        _client = client;
        _logger = logger;
    }

    public async Task<string?> GetAsync(string name, CancellationToken cancellationToken = default)
    {
        try
        {
            var secret = await _client.GetSecretAsync(name, cancellationToken: cancellationToken);
            return secret.Value.Value;
        }
        catch (RequestFailedException ex) when (ex.Status == 404)
        {
            _logger.LogWarning("Secret {SecretName} is not present in Key Vault yet.", name);
            return null;
        }
    }

    public async Task SetAsync(string name, string value, CancellationToken cancellationToken = default)
    {
        await _client.SetSecretAsync(name, value, cancellationToken);
        _logger.LogInformation("Rotated secret {SecretName} in Key Vault.", name);
    }
}

/// <summary>Table Storage fallback for local development, where no Key Vault is configured.</summary>
public sealed class TableRefreshTokenStore : IRefreshTokenStore
{
    private readonly TableClient _table;

    public TableRefreshTokenStore(TableServiceClient serviceClient)
    {
        ArgumentNullException.ThrowIfNull(serviceClient);
        _table = serviceClient.GetTableClient(TableNames.Secrets);
    }

    public async Task<string?> GetAsync(string name, CancellationToken cancellationToken = default)
    {
        try
        {
            var response = await _table.GetEntityAsync<SecretEntity>("secret", name, cancellationToken: cancellationToken);
            return response.Value.Value;
        }
        catch (RequestFailedException ex) when (ex.Status == 404)
        {
            return null;
        }
    }

    public Task SetAsync(string name, string value, CancellationToken cancellationToken = default) =>
        _table.UpsertEntityAsync(
            new SecretEntity { PartitionKey = "secret", RowKey = name, Value = value },
            TableUpdateMode.Replace,
            cancellationToken);
}

/// <summary>
/// Seeds the store from app settings the first time a token is needed, then prefers the stored
/// (rotated) value. This lets the initial refresh token be delivered as a deploy-time secure
/// parameter without pinning the integration to that one value forever.
/// </summary>
public sealed class SeedingRefreshTokenStore : IRefreshTokenStore
{
    private readonly IRefreshTokenStore _inner;
    private readonly IReadOnlyDictionary<string, string> _seeds;
    private readonly ILogger<SeedingRefreshTokenStore> _logger;

    public SeedingRefreshTokenStore(
        IRefreshTokenStore inner,
        IReadOnlyDictionary<string, string> seeds,
        ILogger<SeedingRefreshTokenStore> logger)
    {
        _inner = inner;
        _seeds = seeds;
        _logger = logger;
    }

    public async Task<string?> GetAsync(string name, CancellationToken cancellationToken = default)
    {
        var stored = await _inner.GetAsync(name, cancellationToken);
        if (!string.IsNullOrWhiteSpace(stored))
        {
            return stored;
        }

        if (_seeds.TryGetValue(name, out var seed) && !string.IsNullOrWhiteSpace(seed))
        {
            _logger.LogInformation("Seeding {SecretName} from app settings on first use.", name);
            await _inner.SetAsync(name, seed, cancellationToken);
            return seed;
        }

        return null;
    }

    public Task SetAsync(string name, string value, CancellationToken cancellationToken = default) =>
        _inner.SetAsync(name, value, cancellationToken);
}

public sealed class RefreshTokenStoreFactory
{
    public static IRefreshTokenStore Create(
        IServiceProvider provider,
        IOptions<SecretStoreOptions> secretStoreOptions,
        IReadOnlyDictionary<string, string> seeds)
    {
        var loggerFactory = (ILoggerFactory)provider.GetService(typeof(ILoggerFactory))!;
        var options = secretStoreOptions.Value;

        IRefreshTokenStore inner;
        if (!string.IsNullOrWhiteSpace(options.KeyVaultUri))
        {
            inner = new KeyVaultRefreshTokenStore(
                (SecretClient)provider.GetService(typeof(SecretClient))!,
                loggerFactory.CreateLogger<KeyVaultRefreshTokenStore>());
        }
        else
        {
            inner = new TableRefreshTokenStore((TableServiceClient)provider.GetService(typeof(TableServiceClient))!);
        }

        return new SeedingRefreshTokenStore(inner, seeds, loggerFactory.CreateLogger<SeedingRefreshTokenStore>());
    }
}
