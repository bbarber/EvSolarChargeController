# Architecture decisions and deviations

Four parts of the original specification could not be built as written, because they rest on
assumptions about Tesla's APIs and about cloud free tiers that do not hold. Each is recorded here
with the evidence, what replaced it, and what it costs.

---

## 1. Telemetry ingest cannot be a gateway in front of an HTTP function

**The spec assumed:** vehicles POST protobuf payloads over HTTPS; a gateway terminates mTLS,
validates the client certificate against Tesla's CA, and forwards the body to an HTTP-triggered
function.

**The reasoning behind that was sound.** Azure API Management's Consumption tier really does support
client certificate authentication. For an HTTP API this would have worked exactly as designed. It
fails only because Fleet Telemetry is not an HTTP API.

**What is actually true:** vehicles open a long-lived **WebSocket** and stream binary frames over
it. In Tesla's server ([`server/streaming/server.go`](https://github.com/teslamotors/fleet-telemetry/blob/main/server/streaming/server.go)):

```go
mux.HandleFunc("/", socketServer.ServeBinaryWs(c))   // upgrades to a websocket
...
if r.TLS == nil {                                     // reads the peer cert off TLS state
    return nil, fmt.Errorf("missing_tls_state")
}
if len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
```

The server identifies the vehicle from the client certificate on the TLS connection itself. mTLS has
to terminate **in the process** — a proxy that terminates TLS and forwards a header cannot work, and
Tesla's own Helm chart accordingly uses ingress TLS **passthrough**. Per Tesla's README, *"Vehicles
will connect and stream data directly to the hosted fleet-telemetry server."* Each car presents its
own certificate; there is no single Tesla-owned certificate whose thumbprint could be pinned.

**What was built instead:** Tesla's `fleet-telemetry` binary runs on a VM with the port published
directly. Nothing sits in front of it.

That server dispatches to kafka, kinesis, google pubsub, zmq, mqtt or redis — there is **no HTTP
dispatcher**. Of those, ZeroMQ is the only one needing no broker, so the server publishes to a PUB
socket and the controller subscribes to it.

---

## 2. "Everything free" and "push telemetry" cannot both hold on Azure

**The spec assumed:** the whole system fits inside Azure's free tiers.

**What is actually true:** vehicles hold persistent connections, so the endpoint cannot scale to
zero, and **no always-on container on Azure is free at any size.** The Container Apps free grant is
[180,000 vCPU-s and 360,000 GiB-s per subscription per month](https://azure.microsoft.com/en-us/pricing/details/container-apps/),
while a replica running for 30 days consumes 2,592,000 seconds of wall clock. The grant therefore
covers 0.069 vCPU running continuously, against a platform minimum of 0.25.

An earlier version of this document put the shortfall at $11–18/month, counting only compute. That
was wrong, and the billing data proved it. External TCP ingress requires a VNet-integrated
environment, and such an environment implicitly provisions a **Standard Load Balancer and a static
public IP** — neither of which appears in the Bicep. Measured from the actual subscription:

| Component | Rate | Monthly |
|---|---|---|
| Standard LB rules | $0.025/hr (billed $0.5574 for ~22h on 2026-08-12) | **$18.25** |
| Standard static public IPv4 | $0.005/hr | **$3.65** |
| 0.25 vCPU beyond the grant | $0.000024/vCPU-s active, $0.000003 idle | $1.40–11.23 |
| 0.5 GiB beyond the grant | $0.000003/GiB-s | $2.81 |
| | | **~$26–36/month** |

Windowing the container to daylight hours does not rescue it: the load balancer and IP bill for the
environment's existence, not its usage.

**What was built instead:** the whole system moved to an **Oracle Cloud Always Free** VM, where the
compute, the public IPv4, and 10 TB/month of egress are genuinely $0 with no expiry. The Azure
subscription was emptied.

**What this cost in practice.** Oracle's home region is chosen once and cannot be changed, and
`us-chicago-1` turned out to carry **no E2 shapes at all** — the AMD `VM.Standard.E2.1.Micro` that
most Always Free guidance recommends. Four signals disagreed, and only a launch attempt settled it:

| Check | Verdict on E2.1.Micro |
|---|---|
| `oci compute shape list`, region-wide and AD-scoped | absent |
| Service limit `vm-standard-e2-1-micro-count` | 2, in AD-2 |
| `oci limits resource-availability get` | available: 2 |
| `oci compute image list --shape …` | 46 compatible images |
| **`LaunchInstance`** | **404-NotAuthorizedOrNotFound, as an Administrator** |

The limit entries are vestigial defaults present in every tenancy; the region is built on E4/E5
hardware. That leaves `VM.Standard.A1.Flex` (Ampere, **aarch64**) as the only Always Free shape
available, which is why every container is built for arm64. A1 capacity is itself scarce —
`infra/oracle/launch-retry.sh` exists because "Out of host capacity" is the normal first answer.

---

## 3. `set_charging_amps` needed a proxy in C#. It does not in Go.

**The spec assumed:** the timer calls the Fleet API `set_charging_amps` endpoint with a bearer
token.

**What is actually true:** vehicles built after 2021 require **end-to-end signed commands**. An
unsigned command is rejected by the car itself, silently as far as HTTP is concerned. Signing needs
the application's private key and Tesla's vehicle-command protocol — ECDH session establishment with
the vehicle, AES-GCM encryption, and anti-replay counters.

**What this used to cost.** In C# the only practical answer was to run Tesla's `tesla-http-proxy` as
a second container and POST to it. Implementing the protocol directly would have been roughly 800
lines of cryptographic code with no vehicle to test against, and the failure mode of getting it
subtly wrong is commands accepted by the API and ignored by the car. The proxy in turn needed an
nginx wrapper, because it mandates TLS with no way to disable it while the platform's ingress spoke
plain HTTP to the container, plus a shared secret to gate its public ingress.

**What replaced it.** Tesla publishes the protocol as a Go library. `SetChargingAmps` is a method
([`pkg/vehicle/charge.go:161`](https://github.com/teslamotors/vehicle-command/blob/main/pkg/vehicle/charge.go)):

```go
func (v *Vehicle) SetChargingAmps(ctx context.Context, amps int32) error
```

So the controller signs and sends commands in-process. **The proxy container, the nginx wrapper, the
shared secret, and the extra public port are all gone.** This was the single largest reason to move
the codebase from C# to Go; the language was never the problem, but the ecosystem was.

Only the infotainment domain is started for a session, since charging commands terminate there and
starting the security domain too would mean a second handshake for nothing.

---

## 4. Azure Functions and Table Storage were the wrong shape for one box

**The spec assumed:** a timer-triggered function and an HTTP-triggered ingest function over Table
Storage.

**What is actually true:** on a single VM neither needs a host. The timer is a ticker and the ingest
is a goroutine reading the same in-process store, so roughly 1,265 lines of C# — the Table Storage
repositories, the Functions bindings, and the ZeroMQ-to-HTTP bridge that existed only to cross the
process boundary — had no remaining purpose. State moved to SQLite: a single file, one writer, and
WAL journaling so an unclean shutdown does not corrupt it.

The shared-secret HTTP hop between the bridge and the ingest function disappeared with them.

---

## Resolved from the spec's open-questions list

**Telemetry field names — resolved.** The proto defines both `ChargeState` (field 2) and
`DetailedChargeState` (field 179), and marks the former *"deprecated and not used"*, so
`DetailedChargeState` is authoritative. The fields subscribed to are `ChargeAmps` (49),
`ChargeCurrentRequest` (53), `ChargeCurrentRequestMax` (54), `DetailedChargeState` (179) and
`ChargePortLatch` (118). Each numeric value is accepted as int, long, float, double or string,
because Tesla has shipped these differently across firmware versions.

**Protobuf generation — no longer needed.** Tesla publishes generated Go types at
`github.com/teslamotors/fleet-telemetry/protos`, so the vendored `.proto` file and the codegen step
are gone; the schema tracks Tesla's releases through an ordinary module upgrade.

**Enphase v4 endpoints — resolved.** Token endpoint `https://api.enphaseenergy.com/oauth/token`,
refresh via `grant_type=refresh_token` with HTTP Basic client credentials; data at
`https://api.enphaseenergy.com/api/v4` with an `Authorization: Bearer` header *and* a `key=` query
parameter. Access tokens last a day, **refresh tokens one month** — and refresh tokens rotate on
use, so the new one is written back on every refresh. Leaving the integration idle for over a month
requires a fresh authorization.

**Tesla token refresh — resolved.** Form-encoded `grant_type=refresh_token`, `client_id` and
`refresh_token`, with **no client secret**. Tesla rotates the refresh token on use as well.

**A domain name — resolved.** Free DuckDNS hostnames. `evsolarchargecontroller.duckdns.org` points
at GitHub Pages for the public key; a second name points at the VM for telemetry. DuckDNS supports
TXT records, which is what makes the DNS-01 certificate flow possible; it does **not** support
CNAME, which is why the public key is hosted via A record.

**Vehicle eligibility — resolved.** Fleet Telemetry requires firmware 2023.20.6 or later, and per
Tesla's README only *"some older model S/X"* are unsupported. A 2023 Model Y and a 2019 Model 3 are
both eligible; firmware 2025.20 added even pre-2021 Intel-based S/X. Worth confirming per-VIN with
`tools/tesla-status.py`, which uses `fleet_status` and does not wake the car.

## Still open — needs your input

1. **Service voltage.** The watts → amps conversion assumes 240V split-phase, the common US
   residential case. If the service is anything else, `EVSOLAR_SYSTEM_VOLTAGE` needs changing;
   every target current scales directly with it.
2. **`ChargeAmps` vs `ChargeCurrentRequest`.** Both are subscribed; `ChargeAmps` is preferred with
   the other as fallback. Which one actually tracks the slider in the Tesla app should be confirmed
   against a real vehicle before trusting override detection — this remains the most likely source
   of false positives in the field.
3. **Idle reclamation.** Oracle may reclaim Always Free instances idle for 7 days on CPU, network
   **and** memory, and the memory criterion applies specifically to A1 shapes. A telemetry server
   for two cars will sit under all three. Converting the tenancy to Pay As You Go removes the risk
   and keeps Always Free resources at $0; set a budget alert alongside it.
