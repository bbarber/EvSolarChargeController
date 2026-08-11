# Setup runbook

Order matters. Later steps depend on endpoints created by earlier ones, and a few steps need
physical access to the cars.

Legend: 🧑 needs a human (browser login, phone, or the vehicle) · 🤖 scriptable

---

## 0. Prerequisites

- Azure subscription `EvSolarChargeController` (`ff51dc3e-7430-4235-bce0-0537cd077543`)
- A domain you control, for the telemetry endpoint and the Tesla public key
- `az`, `dotnet` 8, `func` v4, `openssl`, and Docker for local container builds

---

## 1. 🧑 Tesla developer application

1. Register at [developer.tesla.com](https://developer.tesla.com).
2. Create an application. In **Client Details**, choose **Authorization Code and Machine-to-Machine**.
3. Request scopes: `vehicle_device_data`, `vehicle_cmds`, `vehicle_charging_cmds`, plus
   `openid` and `offline_access`.
4. Record the client ID and client secret into `.secrets/tesla.env` (git-ignored):

   ```
   TESLA_CLIENT_ID=...
   TESLA_CLIENT_SECRET=...
   TESLA_REFRESH_TOKEN=
   ```

## 2. 🤖 Generate the application key pair

```bash
mkdir -p .secrets/tesla-keys && cd .secrets/tesla-keys
openssl ecparam -name prime256v1 -genkey -noout -out fleet-key.pem
openssl ec -in fleet-key.pem -pubout -out public-key.pem
```

`fleet-key.pem` is the signing key used by the command proxy. It never leaves Key Vault and the
mounted config share.

## 3. 🧑 Host the public key

Tesla fetches it from:

```
https://<your-domain>/.well-known/appspecific/com.tesla.3p.public-key.pem
```

Either point a domain at the APIM module (`deployApim = true`, paste the PEM into
`teslaPublicKeyPem`) or serve the file from any static host you already run. It must be reachable
over HTTPS with a publicly trusted certificate before the next step.

## 4. 🧑 Register the application and pair the virtual key

1. Generate a partner authentication token and call the Fleet API
   [register](https://developer.tesla.com/docs/fleet-api/authentication/partner-tokens) endpoint
   with your domain.
2. On each car's phone, open `https://tesla.com/_ak/<your-domain>` and approve.
   **This requires being in or near the vehicle with Bluetooth on**, once per car.

## 5. 🧑 Tesla OAuth — get a refresh token

Complete the authorization-code flow in a browser, then put the resulting refresh token in
`.secrets/tesla.env` as `TESLA_REFRESH_TOKEN`. The app rotates it from then on and writes each new
value back to Key Vault.

## 6. 🧑 Enphase — dedicated application

Already done. The app **EvSolarChargeController** on the Watt plan exists, with its credentials in
`.secrets/enphase.env`. It is deliberately separate from the existing PVOutput app so the two do
not share the 1000-calls/month budget.

Still outstanding:

1. Authorize it and exchange the code for a refresh token:

   ```
   https://api.enphaseenergy.com/oauth/authorize?response_type=code&client_id=$ENPHASE_CLIENT_ID&redirect_uri=<your-redirect>
   ```

   The client id is in `.secrets/enphase.env`; the developer portal also shows the fully-formed
   authorization URL.

   ```bash
   curl -X POST "https://api.enphaseenergy.com/oauth/token?grant_type=authorization_code&redirect_uri=<your-redirect>&code=<code>" \
     -u "<client-id>:<client-secret>"
   ```

2. Find the system id:

   ```bash
   curl -H "Authorization: Bearer <access-token>" \
     "https://api.enphaseenergy.com/api/v4/systems?key=<api-key>"
   ```

3. Fill `ENPHASE_REFRESH_TOKEN` and `ENPHASE_SYSTEM_ID` in `.secrets/enphase.env`.

> The Enphase refresh token expires after **one month**. The timer refreshes it well within that,
> but if the app is left switched off longer than a month, repeat this step.

## 7. 🤖 Generate shared secrets

```bash
openssl rand -base64 32   # -> INGEST_SHARED_SECRET
openssl rand -base64 32   # -> PROXY_SHARED_SECRET
```

Put both in `.secrets/tesla.env`, and add them to the GitHub repository secrets
`INGEST_SHARED_SECRET` and `PROXY_SHARED_SECRET` for the deploy workflow.

## 8. 🤖 Deploy the base infrastructure

```bash
az account set --subscription ff51dc3e-7430-4235-bce0-0537cd077543
az group create -n rg-evsolar-prod -l centralus

az deployment group create \
  -g rg-evsolar-prod \
  -f infra/main.bicep \
  -p infra/main.bicepparam \
  -p ingestSharedSecret="$INGEST_SHARED_SECRET" proxySharedSecret="$PROXY_SHARED_SECRET"
```

This creates storage plus tables, Key Vault, Log Analytics, Application Insights, and the Function
App. Container Apps and APIM stay off until their prerequisites exist.

## 9. 🤖 Seed the secrets

```bash
./tools/seed-keyvault.sh <key-vault-name-from-outputs>
```

## 10. 🤖 Deploy the function code

```bash
func azure functionapp publish <function-app-name>
```

Check Application Insights for the startup line listing the schedule, time zone and charge range.
Missing configuration is reported there explicitly.

## 11. 🤖 Build and publish the containers

Run the **Build containers** workflow, or locally:

```bash
docker build -f src/EvSolarChargeController.TelemetryBridge/Dockerfile -t telemetry-bridge .
docker build -f tools/tesla-command-proxy/Dockerfile -t tesla-command-proxy .
```

Copy the resulting image references into `bridgeImage` and `teslaProxyImage` in
`main.bicepparam`.

## 12. 🧑 Prepare the telemetry config share

Create the `telemetry-config` file share in the storage account and upload:

| File | What it is |
|---|---|
| `config.json` | from `tools/fleet-telemetry/config.example.json`, with your hostname |
| `server-cert.pem`, `server-key.pem` | publicly trusted certificate for the telemetry FQDN |
| `tesla-ca.pem` | Tesla's telemetry CA, used to verify the vehicle's client certificate |
| `fleet-key.pem` | the EC private key from step 2 |

Then set `deployContainerApps = true` and re-run step 8.

Point your telemetry DNS record at the Container App's FQDN from the deployment outputs, and
validate with Tesla's
[`check_server_cert.sh`](https://github.com/teslamotors/fleet-telemetry/blob/main/tools/check_server_cert.sh).

## 13. 🧑 Register telemetry on each vehicle

Call `fleet_telemetry_config` **through the command proxy** (it is a signed command), with your
hostname, port 443, the CA chain, and these fields:

| Field | Interval |
|---|---|
| `DetailedChargeState` | 60s |
| `ChargeAmps` | 60s |
| `ChargeCurrentRequest` | 60s |
| `ChargeCurrentRequestMax` | 300s |
| `ChargePortLatch` | 60s |

Poll the GET form of the same endpoint until `synced` is true. Diagnose problems with
`fleet_telemetry_errors`.

## 14. Verify end to end

- `VehicleState` gains a row per VIN once telemetry flows.
- `SolarReadings` gains a row every 20 minutes during daylight.
- `ApiUsage` shows the monthly Enphase count climbing ~30/day.
- Application Insights logs a decision line every cycle.
- Change amps in the Tesla app while charging: within a cycle the logs should report
  `SkipOverrideActive`. Unplug, and it should clear.

---

## Rotating credentials

| Credential | When | How |
|---|---|---|
| Enphase refresh token | automatic, per use | written to Key Vault by the app |
| Tesla refresh token | automatic, per use | written to Key Vault by the app |
| Enphase client secret | if compromised | regenerate in the developer portal, re-run step 9 |
| Shared secrets | any time | regenerate, re-run step 9, redeploy container apps |
| Telemetry TLS certificate | before expiry | replace on the file share, restart the container app |

## Troubleshooting

**Timer fires but nothing happens.** Check the decision reason in App Insights — the common causes
are `SkipNotCharging` (correct: the car is asleep or unplugged) and `SkipOverrideActive`.

**`Enphase monthly call budget exhausted`.** Check the `ApiUsage` table. If the count is far above
~30/day, something is calling outside the daylight window — verify `WEBSITE_TIME_ZONE`.

**Override trips immediately after every command.** The car is likely reporting a different field
than the one being compared, or clamping the request. Compare `ChargeAmps` against
`ChargeCurrentRequest` in the telemetry logs; see open question 3 in
[ARCHITECTURE-DECISIONS.md](ARCHITECTURE-DECISIONS.md).

**Commands return 200 but nothing changes.** The command was not signed. Confirm
`Tesla__CommandMode=Proxy` and that the proxy is reachable.
