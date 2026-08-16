# Setup runbook

Order matters. Later steps depend on endpoints created by earlier ones, and a few steps need
physical access to the cars.

Legend: 🧑 needs a human (browser login, phone, or the vehicle) · 🤖 scriptable

---

## 0. Prerequisites

- An Oracle Cloud tenancy. Its **home region is permanent**, so pick one close to you — and note
  that not every region carries every Always Free shape (see
  [ARCHITECTURE-DECISIONS.md](ARCHITECTURE-DECISIONS.md#2-everything-free-and-push-telemetry-cannot-both-hold-on-azure)).
- Two free [DuckDNS](https://duckdns.org) hostnames (step 0a)
- `oci`, `terraform`, `go` 1.26, `openssl`, and Docker

### 0a. 🧑 DuckDNS hostnames

Two are needed. A DuckDNS name carries one A record, and the two endpoints live at different
addresses.

| Hostname | A record points at | Serves |
|---|---|---|
| `evsolarchargecontroller.duckdns.org` | `185.199.108.153` (GitHub Pages) | the Tesla public key |
| `<something>-tel.duckdns.org` | the VM's public IP | fleet-telemetry, TCP 8443 |

The second one's A record can't be set until the VM exists (step 8), so create the name now and
leave it pointing anywhere.

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
4. Enable billing. Fleet API is pay-per-use against a $10/month discount; pushed telemetry for two
   cars plus a few commands a day sits well inside it, but the account still needs billing set up.
5. Record the client id into `.secrets/tesla.env` (git-ignored):

   ```
   TESLA_CLIENT_ID=...
   TESLA_REFRESH_TOKEN=
   ```

   The client secret is not used by this controller — Tesla's refresh grant takes only `client_id`
   and `refresh_token` — but keep it for the partner-token call in step 4.

## 2. 🤖 Generate the application key pair

```bash
mkdir -p .secrets/tesla-keys && cd .secrets/tesla-keys
openssl ecparam -name prime256v1 -genkey -noout -out fleet-key.pem
openssl ec -in fleet-key.pem -pubout -out public-key.pem
```

`fleet-key.pem` signs every command **and** the telemetry configuration. It is paired to each
vehicle as a virtual key, so losing or replacing it means re-pairing both cars by hand — with
physical access. Back it up somewhere you trust.

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
5. Push. The **Deploy public key site** workflow publishes `site/`.

Verify before moving on — a failed fetch produces an unhelpful error during pairing:

```bash
curl -sI https://evsolarchargecontroller.duckdns.org/.well-known/appspecific/com.tesla.3p.public-key.pem
```

> The workflow refuses to publish if it finds any private key under `site/`, and `.gitignore`
> allows `*.pem` through only for this one path.

## 4. 🧑 Register the application and pair the virtual key

1. Generate a partner authentication token and call the Fleet API
   [register](https://developer.tesla.com/docs/fleet-api/authentication/partner-tokens) endpoint
   with your domain (`tools/tesla-register-partner.sh`).
2. On each car's phone, open `https://tesla.com/_ak/<your-domain>` and approve.
   **This requires being in or near the vehicle with Bluetooth on**, once per car.

## 5. 🧑 Tesla OAuth — get a refresh token

Complete the authorization-code flow in a browser (`tools/tesla-oauth.py`), then put the resulting
refresh token in `.secrets/tesla.env` as `TESLA_REFRESH_TOKEN`. It seeds the database once; the
controller rotates it from then on and the stored copy becomes authoritative.

## 6. 🧑 Enphase — dedicated application

The app **EvSolarChargeController** on the free Watt plan exists, with credentials in
`.secrets/enphase.env`. It is deliberately separate from the existing PVOutput app so the two do
not share the 1000-calls/month budget.

1. Authorize it and exchange the code for a refresh token (`tools/enphase-oauth.py`):

   ```
   https://api.enphaseenergy.com/oauth/authorize?response_type=code&client_id=$ENPHASE_CLIENT_ID&redirect_uri=<your-redirect>
   ```

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

> The Enphase refresh token expires after **one month**. The controller refreshes it well within
> that, but if it is left switched off longer than a month, repeat this step.

## 7. 🤖 Build the network

```bash
cd infra/oracle
terraform init
terraform apply
```

This creates the VCN, internet gateway, route table, security list and subnet. All of it is free.

## 8. 🤖 Launch the instance

Always Free A1 capacity is scarce, and "Out of host capacity" is the normal first answer:

```bash
./launch-retry.sh          # cycles the availability domains until one frees up
```

It stops immediately on any error that is **not** a capacity error, because those do not resolve by
waiting — a 404 means the shape is not offered in the region at all.

Consider converting the tenancy to **Pay As You Go** first. Always Free resources stay $0, it
removes the idle-reclamation risk, and it is widely reported to improve capacity odds. Set a $1
budget alert alongside it.

Then point the telemetry DuckDNS name at the `public_ip` output.

## 9. 🤖 Issue the telemetry certificate

Set the repository variables `TELEMETRY_HOSTNAME`, `ACME_EMAIL`, `TELEMETRY_SSH_USER` (default
`ubuntu`) and `TELEMETRY_DEPLOY_DIR` (default `/opt/evsolar`), and the secrets `DUCKDNS_TOKEN` and
`TELEMETRY_SSH_KEY`. Then run the **Renew telemetry certificate** workflow.

It issues over DNS-01 — nothing answers HTTP on that hostname, since it is a raw TLS listener on
8443 — copies the certificate to the host, and restarts only the telemetry container.

## 10. 🤖 Deploy

On the VM:

```bash
sudo mkdir -p /opt/evsolar && sudo chown ubuntu:ubuntu /opt/evsolar && cd /opt/evsolar
# copy from deploy/: docker-compose.yml, .env (from .env.example), fleet-telemetry/config.json
mkdir -p secrets && cp ~/fleet-key.pem secrets/fleet-key.pem && chmod 600 secrets/fleet-key.pem
docker compose up -d
docker compose logs -f controller
```

The startup log prints the resolved window, limits, and the command key fingerprint. A mismatched
key shows up here rather than as an unexplained rejected command later.

## 11. 🤖 Register telemetry on each vehicle

The configuration must be signed with the command key — the vehicle verifies it against the
published public key before accepting a new destination:

```bash
evsolar-register \
  -host "$TELEMETRY_HOSTNAME" -port 8443 \
  -ca /opt/evsolar/fleet-telemetry/server-cert.pem \
  -vins 7SAYGDEEXPA069171,5YJ3E1EA3KF428848 \
  -client-id "$EVSOLAR_TESLA_CLIENT_ID"

# poll until "synced" is true
evsolar-register -status -vins 7SAYGDEEXPA069171
```

The fields and intervals it requests are in `internal/tesla/telemetry_config.go`. `BatteryLevel` /
`Soc` drive the state-of-charge cap — without them the cap silently never engages.

Diagnose problems with the `fleet_telemetry_errors` endpoint.

## 12. Verify end to end

- `docker compose logs controller` shows a decision line every 20 minutes during daylight.
- The `solar_readings` table gains a row per cycle; `api_usage` climbs ~27/day.
- `vehicle_state` gains a row per VIN once telemetry flows.
- Change amps in the Tesla app while charging: within a cycle the log should report
  `SkipOverrideActive`. Unplug, and it should clear.

```bash
docker compose exec controller sqlite3 /var/lib/evsolar/evsolar.db \
  "select vin, charging_state, charge_amps, last_set_amps, override_active from vehicle_state;"
```

---

## Rotating credentials

| Credential | When | How |
|---|---|---|
| Enphase refresh token | automatic, per use | written to SQLite by the controller |
| Tesla refresh token | automatic, per use | written to SQLite by the controller |
| Enphase client secret | if compromised | regenerate in the portal, update `.env`, restart |
| Telemetry TLS certificate | before expiry | the weekly workflow; or re-run it manually |
| Command key | only if compromised | regenerate, republish the public key, **re-pair both cars** |

The `evsolar-data` volume holds both rotating refresh tokens. Losing it means re-authorizing
Enphase and Tesla by hand, because each provider invalidates the previous token on use.

## Troubleshooting

**Nothing happens on a tick.** Check the decision reason — the common causes are `SkipNotCharging`
(correct: the car is asleep or unplugged) and `SkipOverrideActive`.

**`Enphase monthly call budget exhausted`.** Check `api_usage`. If the count is far above ~27/day,
something is polling outside the daylight window — verify `EVSOLAR_TIMEZONE`.

**Override trips immediately after every command.** The car is likely reporting a different field
than the one being compared, or clamping the request. Compare `ChargeAmps` against
`ChargeCurrentRequest` in the telemetry logs; see open question 2 in
[ARCHITECTURE-DECISIONS.md](ARCHITECTURE-DECISIONS.md).

**Commands return success but nothing changes.** Confirm the key fingerprint in the startup log
matches the key paired to the car, and that the virtual key is still present in the Tesla app.

**No telemetry arrives.** Confirm the endpoint completes a TLS handshake from outside:
`openssl s_client -connect "$TELEMETRY_HOSTNAME:8443"`. Both the VCN security list *and* the
instance's own iptables must allow 8443 — Oracle's Ubuntu images ship an INPUT chain ending in
REJECT, which is the single most common reason a correctly configured endpoint still refuses
connections. `cloud-init.yaml` handles this on a fresh instance.
