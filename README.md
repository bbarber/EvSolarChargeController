# EvSolarChargeController

Automatically matches a Tesla wall connector's charge current to rooftop solar production, and
backs off the moment someone changes amps by hand in the Tesla app.

Solar production is read from the Enphase cloud on a 20-minute cadence during daylight hours; the
target current is the **maximum** amp-equivalent seen in the trailing 60 minutes, which biases
toward overshoot rather than chasing every passing cloud. Vehicle state arrives by push — the car
is never polled, because polling can wake a sleeping vehicle.

## Status

| Piece | State |
|---|---|
| Functions (`TelemetryIngest`, `SolarSyncTimer`) | Implemented, 69 unit tests passing |
| Decision + override logic | Implemented and tested |
| Enphase v4 client (OAuth refresh, quota guard) | Implemented, not yet exercised against the live API |
| Tesla Fleet client (`set_charging_amps`) | Implemented, not yet exercised against a vehicle |
| Bicep IaC | Compiles clean, **not yet deployed** |
| Telemetry bridge + command proxy containers | Implemented, **not yet built or run** |
| One-time OAuth / virtual-key setup | Not started — needs a human, see [docs/SETUP.md](docs/SETUP.md) |

Nothing here has run against real hardware yet. See
[docs/ARCHITECTURE-DECISIONS.md](docs/ARCHITECTURE-DECISIONS.md) for two places where the original
design turned out not to match how Tesla's APIs actually work, and what replaced them.

## Architecture

```
Tesla vehicle
     │  mutual-TLS WebSocket, protobuf records
     ▼
Azure Container Apps ── fleet-telemetry (Tesla's server, terminates mTLS)
                     └─ bridge sidecar ── ZeroMQ on loopback ──┐
                                                               │ HTTPS + shared secret
                                                               ▼
                                          Azure Function: TelemetryIngest
                                               │ decode protobuf, fold into state
                                               ▼
                                          Table Storage
                                            • VehicleState   (one row per VIN)
                                            • SolarReadings  (rolling 60 min)
                                            • ApiUsage       (monthly call counter)
                                               ▲
                                               │
Enphase cloud ◀── Azure Function: SolarSyncTimer (every 20 min, 09:00–18:00 America/Chicago)
                                               │
                                               ▼
                     Container Apps ── tesla-http-proxy (signs commands)
                                               │
                                               ▼
                                     Tesla Fleet API: set_charging_amps
```

### The control loop

Every 20 minutes inside the daylight window, `SolarSyncTimer`:

1. Polls Enphase for current production, converts watts to amps at the configured service voltage,
   and stores the reading.
2. Prunes readings older than 60 minutes and takes the maximum of what remains.
3. Reads the vehicle's last-known state and decides:
   - no telemetry, or telemetry older than 6 hours → **skip** (the car is probably asleep)
   - manual override active → **skip** until the car unplugs
   - not actively charging → **skip**, and critically, send nothing that could wake it
   - already at the target → **skip** the redundant command
   - otherwise → clamp into `[MinChargeAmps, MaxChargeAmps]` and send `set_charging_amps`
4. Records what it set, so the next telemetry frame can be compared against it.

Every decision is logged to Application Insights with its reason.

### Override detection

`TelemetryIngest` compares each reported charge current against the value this controller last set.
A mismatch means a human moved the slider, so automatic adjustment stops until the connector comes
out. Three details keep that from misfiring:

- **Settle window.** Mismatches within 3 minutes of our own command are ignored, because telemetry
  emitted before the command landed still carries the old value.
- **Vehicle-reported ceiling.** If the car reports a maximum below our configured one, targets are
  capped to it — otherwise every cycle would look like a mismatch.
- **Unplug, not pause.** Charging completing or stopping does *not* clear an override; only a
  disconnected state or a disengaged charge-port latch does.

## Rate limits

The Enphase free "Watt" plan allows 10 calls/minute and **1000 calls/month**. At 3 calls/hour
across a 9-hour window, a 31-day month costs 837 calls. That is a thin margin, so:

- Every call is counted in the `ApiUsage` table and logged with its running total.
- A configurable budget (`Enphase:MonthlyCallBudget`, default 950) hard-stops polling before the
  real cap is hit.
- Failures are **never retried within a run**. A missed cycle is harmless; a retry storm is not.
- The daylight window is enforced in code as well as in the cron expression, so a lost
  `WEBSITE_TIME_ZONE` setting cannot quietly start burning calls overnight.

## Repository layout

```
infra/                                  Bicep IaC
  main.bicep, main.bicepparam           top-level orchestration + non-secret parameters
  modules/                              storage, keyvault, functionapp, containerapps, apim, rbac
  policies/                             APIM policy XML
src/EvSolarChargeController.Functions/  the two Azure Functions and their domain logic
  Protos/vehicle_data.proto             vendored from teslamotors/fleet-telemetry
src/EvSolarChargeController.TelemetryBridge/  ZeroMQ -> HTTP sidecar
tests/EvSolarChargeController.Tests/    unit tests
tools/                                  secret seeding, container images, fleet-telemetry config
docs/                                   setup runbook and architecture decisions
```

## Local development

```bash
dotnet restore
dotnet build
dotnet test

cp src/EvSolarChargeController.Functions/local.settings.example.json \
   src/EvSolarChargeController.Functions/local.settings.json
# fill in credentials, then:
cd src/EvSolarChargeController.Functions && func start
```

`local.settings.json` and everything under `.secrets/` are git-ignored.

## Deploying

```bash
az deployment group create -g rg-evsolar-prod -f infra/main.bicep -p infra/main.bicepparam
./tools/seed-keyvault.sh <key-vault-name>

# Always redeploy the code after an infrastructure deploy — see the note below.
dotnet publish src/EvSolarChargeController.Functions -c Release -o ./publish
cd publish && zip -qr ../app.zip . && cd ..
az functionapp deployment source config-zip -g rg-evsolar-prod -n <function-app-name> --src app.zip
```

> **Infrastructure deploys wipe the code pointer.** Bicep's `siteConfig.appSettings` replaces the
> whole settings collection, which removes the `WEBSITE_RUN_FROM_PACKAGE` value that zip deployment
> sets. The app then starts cleanly with **zero functions loaded** and no obvious error. Redeploying
> the code restores it.
>
> `func azure functionapp publish` does not work from the project directory here: the generated
> `obj/.../WorkerExtensions.csproj` makes it see two projects and refuse. Publish with `dotnet` and
> deploy the zip instead.

Or use the `Deploy infrastructure` and `Deploy functions` GitHub Actions workflows, which
authenticate with OIDC federated credentials rather than stored secrets.

The one-time Tesla and Enphase account setup — key generation, virtual-key pairing, OAuth
authorization, telemetry registration — cannot be expressed as infrastructure and is documented
step by step in [docs/SETUP.md](docs/SETUP.md).

## Configuration

Settings bind from app settings using `Section__Key` naming.

| Setting | Default | Purpose |
|---|---|---|
| `Charging__SystemVoltage` | 240 | Watts → amps divisor |
| `Charging__MinChargeAmps` | 5 | Connector minimum |
| `Charging__MaxChargeAmps` | 16 | Matched to array peak output |
| `Charging__LookbackWindow` | 60 min | Trailing window for the max |
| `Charging__OverrideSettleWindow` | 3 min | Grace period after our own command |
| `PollingWindow__TimeZone` | America/Chicago | Interprets the window hours |
| `PollingWindow__StartHourLocal` / `EndHourLocal` | 9 / 18 | Daylight window |
| `Enphase__MonthlyCallBudget` | 950 | Hard stop below the 1000/month cap |
| `Tesla__CommandMode` | Proxy | `Direct` only works for pre-2021 S/X |

Credentials live in Key Vault and are referenced from app settings; no secret value appears in a
template, a parameters file, or this repository.
