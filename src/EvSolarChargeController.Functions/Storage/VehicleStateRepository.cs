using Azure;
using Azure.Data.Tables;
using Microsoft.Extensions.Logging;

namespace EvSolarChargeController.Functions.Storage;

public interface IVehicleStateRepository
{
    Task<VehicleStateEntity?> GetAsync(string vin, CancellationToken cancellationToken = default);

    /// <summary>Returns every tracked vehicle row.</summary>
    Task<IReadOnlyList<VehicleStateEntity>> GetAllAsync(CancellationToken cancellationToken = default);

    Task UpsertAsync(VehicleStateEntity entity, CancellationToken cancellationToken = default);

    /// <summary>
    /// Read-modify-write with optimistic concurrency, retried on 412. Telemetry ingest and the
    /// timer can both touch the same row, so blind upserts would lose updates.
    /// </summary>
    Task<VehicleStateEntity> MutateAsync(
        string vin,
        Func<VehicleStateEntity, VehicleStateEntity> mutate,
        DateTimeOffset now,
        CancellationToken cancellationToken = default);
}

public sealed class VehicleStateRepository : IVehicleStateRepository
{
    private const int MaxConcurrencyRetries = 4;

    private readonly TableClient _table;
    private readonly ILogger<VehicleStateRepository> _logger;

    public VehicleStateRepository(TableServiceClient serviceClient, ILogger<VehicleStateRepository> logger)
    {
        ArgumentNullException.ThrowIfNull(serviceClient);
        _table = serviceClient.GetTableClient(TableNames.VehicleState);
        _logger = logger;
    }

    public async Task<VehicleStateEntity?> GetAsync(string vin, CancellationToken cancellationToken = default)
    {
        try
        {
            var response = await _table.GetEntityAsync<VehicleStateEntity>(
                vin,
                VehicleStateEntity.StateRowKey,
                cancellationToken: cancellationToken);
            return response.Value;
        }
        catch (RequestFailedException ex) when (ex.Status == 404)
        {
            return null;
        }
    }

    public async Task<IReadOnlyList<VehicleStateEntity>> GetAllAsync(CancellationToken cancellationToken = default)
    {
        var results = new List<VehicleStateEntity>();
        var query = _table.QueryAsync<VehicleStateEntity>(
            e => e.RowKey == VehicleStateEntity.StateRowKey,
            cancellationToken: cancellationToken);

        await foreach (var entity in query)
        {
            results.Add(entity);
        }

        return results;
    }

    public Task UpsertAsync(VehicleStateEntity entity, CancellationToken cancellationToken = default) =>
        _table.UpsertEntityAsync(entity, TableUpdateMode.Replace, cancellationToken);

    public async Task<VehicleStateEntity> MutateAsync(
        string vin,
        Func<VehicleStateEntity, VehicleStateEntity> mutate,
        DateTimeOffset now,
        CancellationToken cancellationToken = default)
    {
        for (var attempt = 0; ; attempt++)
        {
            var existing = await GetAsync(vin, cancellationToken);
            var isNew = existing is null;
            var entity = mutate(existing ?? VehicleStateEntity.CreateNew(vin, now));

            try
            {
                if (isNew)
                {
                    await _table.AddEntityAsync(entity, cancellationToken);
                }
                else
                {
                    await _table.UpdateEntityAsync(entity, entity.ETag, TableUpdateMode.Replace, cancellationToken);
                }

                return entity;
            }
            catch (RequestFailedException ex) when (
                (ex.Status == 412 || ex.Status == 409) && attempt < MaxConcurrencyRetries)
            {
                _logger.LogDebug(
                    "Concurrent update to VehicleState for {Vin} (HTTP {Status}); retrying (attempt {Attempt}).",
                    vin,
                    ex.Status,
                    attempt + 1);
            }
        }
    }
}
