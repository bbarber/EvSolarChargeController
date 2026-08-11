# Bridge sidecar: subscribes to fleet-telemetry's ZeroMQ dispatcher and relays records to the
# TelemetryIngest Function over HTTPS.
#
# Build from the repository root:
#   docker build -f src/EvSolarChargeController.TelemetryBridge/Dockerfile -t telemetry-bridge .

FROM mcr.microsoft.com/dotnet/sdk:8.0 AS build
WORKDIR /src

COPY src/EvSolarChargeController.TelemetryBridge/EvSolarChargeController.TelemetryBridge.csproj \
     src/EvSolarChargeController.TelemetryBridge/
RUN dotnet restore src/EvSolarChargeController.TelemetryBridge/EvSolarChargeController.TelemetryBridge.csproj

COPY src/EvSolarChargeController.TelemetryBridge/ src/EvSolarChargeController.TelemetryBridge/
RUN dotnet publish src/EvSolarChargeController.TelemetryBridge/EvSolarChargeController.TelemetryBridge.csproj \
      -c Release \
      -o /app \
      --no-restore

FROM mcr.microsoft.com/dotnet/runtime:8.0 AS runtime
WORKDIR /app

# NetMQ is a managed ZeroMQ implementation, so no native libzmq package is needed here.
COPY --from=build /app .

# Runs unprivileged; the bridge only needs an outbound socket and an outbound HTTPS call.
USER $APP_UID

ENTRYPOINT ["dotnet", "EvSolarChargeController.TelemetryBridge.dll"]
