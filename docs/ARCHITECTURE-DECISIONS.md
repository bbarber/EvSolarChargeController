# Architecture decisions and deviations

Two parts of the original specification could not be built as written, because they rest on
assumptions about Tesla's APIs that do not hold. Both are recorded here with the evidence, what
was built instead, and what it costs.

Everything else in the spec was implemented as specified.

---

## 1. Telemetry ingest cannot be APIM in front of an HTTP Function

**The spec assumed:** vehicles POST protobuf payloads over HTTPS; API Management terminates mTLS,
validates the client certificate against Tesla's CA, and forwards the body to an HTTP-triggered
Function.

**What is actually true:** Tesla Fleet Telemetry is not request/response HTTP. Vehicles open a
long-lived **WebSocket** connection and stream binary frames over it. In Tesla's server
([`server/streaming/server.go`](https://github.com/teslamotors/fleet-telemetry/blob/main/server/streaming/server.go)):

```go
mux.HandleFunc("/", socketServer.ServeBinaryWs(c))   // upgrades to a websocket
...
if r.TLS == nil {                                     // reads the peer cert off TLS state
    return nil, fmt.Errorf("missing_tls_state")
}
if len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
```

The server identifies the vehicle from the client certificate on the TLS connection itself. That
means mTLS has to terminate *in the process*, not at a gateway — a proxy that terminates TLS and
forwards a header cannot work, and Tesla's own Helm chart accordingly uses ingress TLS
**passthrough**. An HTTP-triggered Function is not a WebSocket server and cannot be the endpoint
regardless.

**What was built instead:** Tesla's `fleet-telemetry` server runs in Azure Container Apps, in a
VNet-integrated environment with **TCP ingress** so the handshake reaches the container untouched.

That server dispatches records to kafka, kinesis, google pubsub, zmq, redis or mqtt — there is
**no HTTP dispatcher**. Of those, ZeroMQ is the only one needing no broker and therefore no
additional paid Azure resource, so the server publishes to a loopback PUB socket and a small .NET
sidecar in the same Container App relays each record to `TelemetryIngest` over HTTPS.

**What this preserves:** the Function's interface is exactly what the spec described — an
HTTP-triggered function receiving a raw Tesla protobuf payload, authenticated by a shared secret
header. All the ingest logic, override detection and storage behaviour are unchanged.

**What it costs:**

- One always-on Container Apps replica (vehicles hold persistent connections, so it cannot scale to
  zero). This sits inside the Container Apps monthly free grant at this scale, but it is no longer
  "serverless".
- A VNet, required for TCP ingress.
- A publicly trusted TLS certificate for the telemetry FQDN, which must be renewed. Container Apps'
  free managed certificates do **not** apply to TCP ingress.

**Unvalidated:** external TCP ingress on a VNet-integrated Container Apps environment is configured
per Microsoft's documentation but has not been deployed. If it turns out not to be available, the
fallback is a small always-on VM with the same container, which would leave the free tier.

**APIM's remaining role:** hosting the Tesla third-party public key at
`/.well-known/appspecific/com.tesla.3p.public-key.pem` on a custom domain — a genuine prerequisite
for virtual-key pairing, served inside the Consumption tier's free allotment. It is off by default
(`deployApim = false`) since it needs a domain.

---

## 2. `set_charging_amps` cannot be called directly

**The spec assumed:** the timer calls the Fleet API `set_charging_amps` endpoint with a bearer
token.

**What is actually true:** vehicles built after 2021 (everything except pre-2021 Model S/X and
business-owned fleet vehicles) require **end-to-end signed commands**. An unsigned command is
rejected by the car itself, silently as far as HTTP is concerned. Signing requires the
application's private key and Tesla's vehicle-command protocol — ECDH session establishment with
the vehicle, AES-GCM encryption, and anti-replay counters.

**What was built instead:** `TeslaFleetClient` has two modes.

- `Proxy` (default) — POSTs to a `tesla-http-proxy` instance, which signs the command and forwards
  it. Request and response shapes are identical to Fleet API, so only the base URL differs.
- `Direct` — POSTs straight to Fleet API, for pre-2021 S/X or fleet vehicles.

The proxy runs as a second Container App, scaled to zero between commands (a handful a day). Its
image wraps `tesla-http-proxy` behind nginx, because the proxy mandates TLS with no way to disable
it while Container Apps' HTTP ingress speaks plain HTTP to the container; nginx accepts the ingress
traffic on 8080 and re-wraps it to the proxy on loopback. A shared-secret header gates the public
ingress.

**The alternative considered and rejected:** implementing command signing in C# directly would keep
everything serverless, but it is roughly 800 lines of cryptographic protocol code with no vehicle
available to test against. The failure mode of getting it subtly wrong is commands that are
accepted by the API and ignored by the car.

---

## Resolved from the spec's open-questions list

**Telemetry field names — resolved.** The proto defines both `ChargeState` (field 2) and
`DetailedChargeState` (field 179), and marks the former explicitly:

```proto
// ChargingState is deprecated and not used
enum ChargingState { ... }
```

So `DetailedChargeState` is authoritative. The fields subscribed to are `ChargeAmps` (49),
`ChargeCurrentRequest` (53), `ChargeCurrentRequestMax` (54), `DetailedChargeState` (179) and
`ChargePortLatch` (118). The decoder accepts each numeric value as int, long, float, double or
string, because Tesla has shipped these differently across firmware versions.

**Enphase v4 endpoints — resolved.** Token endpoint `https://api.enphaseenergy.com/oauth/token`,
refresh via `grant_type=refresh_token` with HTTP Basic client credentials; data at
`https://api.enphaseenergy.com/api/v4` with an `Authorization: Bearer` header *and* a `key=` query
parameter. Access tokens last a day, **refresh tokens one month** — and refresh tokens rotate on
use, so the new one is written back to Key Vault on every refresh. Leaving the integration idle for
over a month requires a fresh authorization.

## Still open — needs your input

1. **Service voltage.** The watts → amps conversion assumes 240V split-phase, the common US
   residential case. If the service is anything else, `Charging__SystemVoltage` needs changing;
   every target current scales directly with it.
2. **A domain name.** Needed for the telemetry FQDN and for hosting the Tesla public key. Nothing
   in the Tesla path can be registered without one.
3. **`ChargeAmps` vs `ChargeCurrentRequest`.** Both are subscribed; `ChargeAmps` is preferred with
   the other as fallback. Which one actually tracks the app's slider should be confirmed against a
   real vehicle before trusting override detection — this is the most likely source of false
   positives in the field.
4. **Both VINs.** Currently empty in the parameters file.
