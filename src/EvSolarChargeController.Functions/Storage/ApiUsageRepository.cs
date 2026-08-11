using Azure;
using Azure.Data.Tables;
using Microsoft.Extensions.Logging;

namespace EvSolarChargeController.Functions.Storage;

public interface IApiUsageRepository
{
    /// <summary>Calls already made against the provider in the calendar month containing <paramref name="now"/>.</summary>
    Task<int> GetMonthlyCountAsync(string provider, DateTimeOffset now, CancellationToken cancellationToken = default);

    /// <summary>Records one outbound call. Returns the new running total for the month.</summary>
    Task<int> RecordCallAsync(string provider, DateTimeOffset now, bool succeeded, CancellationToken cancellationToken = default);
}

/// <summary>
/// Tracks outbound third-party API usage so the Enphase Watt plan's 1000 calls/month cap can be
/// enforced locally rather than discovered via a 429 at the worst moment.
/// </summary>
public sealed class ApiUsageRepository : IApiUsageRepository
{
    public const string EnphaseProvider = "enphase";

    private const int MaxConcurrencyRetries = 5;

    private readonly TableClient _table;
    private readonly ILogger<ApiUsageRepository> _logger;

    public ApiUsageRepository(TableServiceClient serviceClient, ILogger<ApiUsageRepository> logger)
    {
        ArgumentNullException.ThrowIfNull(serviceClient);
        _table = serviceClient.GetTableClient(TableNames.ApiUsage);
        _logger = logger;
    }

    public async Task<int> GetMonthlyCountAsync(string provider, DateTimeOffset now, CancellationToken cancellationToken = default)
    {
        var entity = await TryGetAsync(provider, ApiUsageEntity.RowKeyFor(now), cancellationToken);
        return entity?.CallCount ?? 0;
    }

    public async Task<int> RecordCallAsync(
        string provider,
        DateTimeOffset now,
        bool succeeded,
        CancellationToken cancellationToken = default)
    {
        var rowKey = ApiUsageEntity.RowKeyFor(now);

        for (var attempt = 0; ; attempt++)
        {
            var existing = await TryGetAsync(provider, rowKey, cancellationToken);
            var entity = existing ?? new ApiUsageEntity { PartitionKey = provider, RowKey = rowKey };

            entity.CallCount++;
            if (!succeeded)
            {
                entity.FailureCount++;
            }

            entity.LastCallAt = now;

            try
            {
                if (existing is null)
                {
                    await _table.AddEntityAsync(entity, cancellationToken);
                }
                else
                {
                    await _table.UpdateEntityAsync(entity, entity.ETag, TableUpdateMode.Replace, cancellationToken);
                }

                return entity.CallCount;
            }
            catch (RequestFailedException ex) when ((ex.Status == 412 || ex.Status == 409) && attempt < MaxConcurrencyRetries)
            {
                _logger.LogDebug("Contended usage counter for {Provider}; retrying (attempt {Attempt}).", provider, attempt + 1);
            }
        }
    }

    private async Task<ApiUsageEntity?> TryGetAsync(string provider, string rowKey, CancellationToken cancellationToken)
    {
        try
        {
            var response = await _table.GetEntityAsync<ApiUsageEntity>(provider, rowKey, cancellationToken: cancellationToken);
            return response.Value;
        }
        catch (RequestFailedException ex) when (ex.Status == 404)
        {
            return null;
        }
    }
}
