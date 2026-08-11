namespace EvSolarChargeController.Functions.Storage;

public static class TableNames
{
    public const string VehicleState = "VehicleState";
    public const string SolarReadings = "SolarReadings";
    public const string ApiUsage = "ApiUsage";
    public const string Secrets = "Secrets";

    public static readonly IReadOnlyList<string> All = new[] { VehicleState, SolarReadings, ApiUsage, Secrets };
}
