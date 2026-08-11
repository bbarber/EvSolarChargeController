using Azure;
using Azure.Data.Tables;
using Microsoft.Extensions.Logging;

namespace EvSolarChargeController.Functions.Storage;

/// <summary>Result of folding the trailing window down to a single target figure.</summary>
public sealed record SolarWindowSummary(double? MaxAmps, double? MaxWatts, int ReadingCount, int PrunedCount);

public interface ISolarReadingsRepository
{
    Task AddAsync(SolarReadingEntity reading, CancellationToken cancellationToken = default);

    /// <summary>
    /// Deletes readings older than the lookback window and returns the maximum amp equivalent
    /// across what remains. Spans today's and yesterday's partitions so a window straddling
    /// midnight UTC still sees the full hour.
    /// </summary>
    Task<SolarWindowSummary> PruneAndSummarizeAsync(
        DateTimeOffset now,
        TimeSpan lookback,
        CancellationToken cancellationToken = default);
}

public sealed class SolarReadingsRepository : ISolarReadingsRepository
{
    private readonly TableClient _table;
    private readonly ILogger<SolarReadingsRepository> _logger;

    public SolarReadingsRepository(TableServiceClient serviceClient, ILogger<SolarReadingsRepository> logger)
    {
        ArgumentNullException.ThrowIfNull(serviceClient);
        _table = serviceClient.GetTableClient(TableNames.SolarReadings);
        _logger = logger;
    }

    public Task AddAsync(SolarReadingEntity reading, CancellationToken cancellationToken = default) =>
        _table.UpsertEntityAsync(reading, TableUpdateMode.Replace, cancellationToken);

    public async Task<SolarWindowSummary> PruneAndSummarizeAsync(
        DateTimeOffset now,
        TimeSpan lookback,
        CancellationToken cancellationToken = default)
    {
        var cutoff = now - lookback;

        // Only two partitions can hold rows inside a <= 24h window: today's and yesterday's.
        var partitions = new HashSet<string>(StringComparer.Ordinal)
        {
            SolarReadingEntity.PartitionKeyFor(now),
            SolarReadingEntity.PartitionKeyFor(cutoff),
        };

        double? maxAmps = null;
        double? maxWatts = null;
        var kept = 0;
        var stale = new List<SolarReadingEntity>();

        foreach (var partition in partitions)
        {
            var query = _table.QueryAsync<SolarReadingEntity>(
                e => e.PartitionKey == partition,
                cancellationToken: cancellationToken);

            await foreach (var reading in query)
            {
                if (reading.ReadingAt < cutoff)
                {
                    stale.Add(reading);
                    continue;
                }

                kept++;
                if (maxAmps is null || reading.AmpsEquivalent > maxAmps)
                {
                    maxAmps = reading.AmpsEquivalent;
                    maxWatts = reading.WattsProduced;
                }
            }
        }

        var pruned = await DeleteAsync(stale, cancellationToken);

        return new SolarWindowSummary(maxAmps, maxWatts, kept, pruned);
    }

    private async Task<int> DeleteAsync(IReadOnlyList<SolarReadingEntity> entities, CancellationToken cancellationToken)
    {
        var deleted = 0;

        foreach (var entity in entities)
        {
            try
            {
                await _table.DeleteEntityAsync(entity.PartitionKey, entity.RowKey, entity.ETag, cancellationToken);
                deleted++;
            }
            catch (RequestFailedException ex) when (ex.Status is 404 or 412)
            {
                // Already gone or changed underneath us — pruning is best-effort housekeeping.
                _logger.LogDebug("Skipped pruning {PartitionKey}/{RowKey}: HTTP {Status}.", entity.PartitionKey, entity.RowKey, ex.Status);
            }
        }

        return deleted;
    }
}
