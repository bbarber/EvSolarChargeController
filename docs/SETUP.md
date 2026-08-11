# Setup runbook

Order matters. Later steps depend on endpoints created by earlier ones, and a few steps need
physical access to the cars.

Legend: 🧑 needs a human (browser login, phone, or the vehicle) · 🤖 scriptable

---

## 0. Prerequisites

- Azure subscription `EvSolarChargeController` (`ff51dc3e-7430-4235-bce0-0537cd077543`)
- Two free [DuckDNS](https://duckdns.org) hostnames (see step 0a)
- `az`, `dotnet` 8, `func` v4, `openssl`, and Docker for local container builds

### 0a. 🧑 DuckDNS hostnames

Two are needed. A DuckDNS name carries one A record, and the two endpoints live at different
addresses.

| Hostname | A record points at | Serves |
|---|---|---|
| `evsolarchargecontroller.duckdns.org` | `185.199.108.153` (GitHub Pages) | the Tesla public key |
| `<something>-tel.duckdns.org` | the `telemetryStaticIp` deployment output | fleet-telemetry, TCP 8443 |

The second one's A record can't be set until the Container Apps environment exists (step 12), so
create the name now and leave it pointing anywhere.

Save the account token — one token covers all your domains:

```
# .secrets/duckdns.env  (git-ignored)
DUCKDNS_TOKEN=...
```

DuckDNS supports TXT records, which is what lets Let's Encrypt issue the telemetry certificate over
DNS-01. It does not support CNAME, which is why the public key is hosted on GitHub Pages (reachable
by A record) rather than a service needing CNAME validation.

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

## 3. 🤖 Host the public key on GitHub Pages

Tesla fetches it from:

```
https://<app-domain>/.well-known/appspecific/com.tesla.3p.public-key.pem
```

1. Copy the **public** half into the published site:

   ```bash
   cp .secrets/tesla-keys/public-key.pem \
      site/.well-known/appspecific/com.tesla.3p.public-key.pem
   ```

2. Set the repository variable `TESLA_APP_DOMAIN` to `evsolarchargecontroller.duckdns.org`.
3. In repository **Settings → Pages**, set the source to **GitHub Actions**.
4. Point the DuckDNS A record for that name at `185.199.108.153`.
5. Push. The **Deploy public key site** workflow publishes `site/` and GitHub issues a certificate
   automatically (it can take a few minutes on first setup).

Verify before moving on — a failed fetch produces an unhelpful error during pairing:

```bash
curl -sI https://evsolarchargecontroller.duckdns.org/.well-known/appspecific/com.tesla.3p.public-key.pem
```

> The workflow refuses to publish if it finds any private key under `site/`, and `.gitignore`
> allows `*.pem` through only for this one path.

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

`server-cert.pem` and `server-key.pem` are produced by the certificate workflow below — upload the
rest by hand, then set `deployContainerApps = true` and re-run step 8.

### 12a. 🤖 Point DNS and issue the telemetry certificate

1. Take `telemetryStaticIp` from the deployment outputs and set the DuckDNS A record for your
   telemetry hostname to it.
2. Set these repository variables and secrets:

   | Name | Kind | Value |
   |---|---|---|
   | `TELEMETRY_HOSTNAME` | variable | e.g. `evsolarchargecontroller-tel.duckdns.org` |
   | `TELEMETRY_APP_NAME` | variable | container app name from the outputs |
   | `STORAGE_ACCOUNT_NAME` | variable | from the outputs |
   | `RESOURCE_GROUP` | variable | `rg-evsolar-prod` |
   | `ACME_EMAIL` | variable | your email, for expiry notices |
   | `DUCKDNS_TOKEN` | secret | from `.secrets/duckdns.env` |

3. Run the **Renew telemetry certificate** workflow. It issues over DNS-01, uploads
   `server-cert.pem` / `server-key.pem` to the config share, and restarts the app. It reruns
   weekly and no-ops until the certificate is within 30 days of expiry.

4. Validate with Tesla's
   [`check_server_cert.sh`](https://github.com/teslamotors/fleet-telemetry/blob/main/tools/check_server_cert.sh),
   using **port 8443**.

## 13. 🧑 Register telemetry on each vehicle

Call `fleet_telemetry_config` **through the command proxy** (it is a signed command), with your
telemetry hostname, **port 8443**, the full Let's Encrypt chain as `ca`, and these fields:

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
