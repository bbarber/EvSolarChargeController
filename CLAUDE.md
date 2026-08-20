# CLAUDE.md

Context for agents working on this repository.

**This repo is public.** No OCIDs, IP addresses, tokens, or VINs belong in any tracked file. Every
credential lives in `.secrets/` (git-ignored), `~/.oci/`, `~/.ssh/`, or `deploy/.env` on the host.
This file points at them; it never contains them.

---

## What this is

A controller that matches a Tesla's charge current to rooftop solar production, and backs off when
someone changes amps by hand in the Tesla app. Two hard constraints shape everything:

1. **Free.** Oracle Cloud Always Free, Enphase's free "Watt" plan (1000 API calls/month), GitHub
   Pages, GHCR, DuckDNS. Nothing costs money. Azure was abandoned because no always-on container
   there is free at any size — see `docs/ARCHITECTURE-DECISIONS.md`.
2. **The car is never polled.** Polling can wake a sleeping vehicle. All vehicle state arrives by
   push over Tesla's Fleet Telemetry.

Solar production *is* polled — that is a different API with a different budget.

---

## Where things run

One Oracle Cloud Always Free VM (Ampere A1, **arm64**), three containers:

| Container | Role |
|---|---|
| `fleet-telemetry` | Tesla's own server. Vehicles open a mutual-TLS WebSocket to it on **8443**. |
| `controller` | This project. Subscribes to fleet-telemetry over ZeroMQ, polls Enphase, signs and sends commands. |
| `public-key` | nginx serving one file on **443**: the app's public key, which Tesla fetches. |

Commands are signed **in-process** using `teslamotors/vehicle-command` as a Go library. There is no
`tesla-http-proxy` — removing it was the main reason this project moved from C# to Go.

---

## Local development

```bash
# The ZeroMQ binding is cgo. Without these the telemetry package will not build.
brew install zeromq pkg-config     # or: apt-get install libzmq3-dev pkg-config

go build ./...
go test ./...
gofmt -l ./cmd ./internal          # must print nothing
go vet ./...
```

### Simulations

Two suites drive the real decision engine through whole simulated days. They dump JSON for plotting
when given an output path:

```bash
SIM_OUT=/tmp/sim.json    go test ./internal/domain/ -run Simulation -v
PLUGIN_OUT=/tmp/plug.json go test ./internal/domain/ -run PlugsIn -v
```

The solar model is a **half-sine and overstates evening production** — it ignores air-mass losses,
so it puts the 5A crossing near 19:00 when reality is closer to 17:50. Do not tune window
boundaries against it. Use real Enphase data.

---

## Cloud access

### Oracle Cloud CLI

Config lives at `~/.oci/config` (profile `DEFAULT`) with the API private key beside it. If it is
missing, regenerate:

```bash
openssl genrsa -out ~/.oci/oci_api_key.pem 2048 && chmod 600 ~/.oci/oci_api_key.pem
openssl rsa -pubout -in ~/.oci/oci_api_key.pem -out ~/.oci/oci_api_key_public.pem
openssl rsa -pubout -outform DER -in ~/.oci/oci_api_key.pem | openssl md5 -c   # fingerprint
```

Paste the **public** key into the console under *My profile → API keys → Add API key*; it returns a
config snippet with the user OCID, tenancy OCID and region. Then:

```bash
export SUPPRESS_LABEL_WARNING=True     # otherwise every command prints a key-label warning
oci iam region list --query 'data[0]' >/dev/null && echo ok
```

New API keys take a few minutes to propagate; sporadic `401 NotAuthenticated` right after adding
one is expected. Retry rather than debugging the config.

The tenancy is **Pay As You Go**. Always Free resources still cost $0; PAYG removes the idle-
reclamation risk and appears to improve A1 capacity odds. Two guardrails exist:

- a **$1 budget** with an alert at $0.01 — alerts only, never blocks
- a **quota policy** `evsolar-always-free-only` that *does* block: every paid compute shape is
  zeroed, with only the Always Free A1 allowance (2 OCPU / 12 GB) permitted

Quota names are **not** the same as limit names. Memory lives in the `compute-memory` family, cores
in `compute-core`. The API validates statements on create, so a bad name fails loudly — test a
throwaway policy before editing the real one.

### SSH to the host

```bash
ssh -i ~/.ssh/oci_evsolar ubuntu@<public_ip>     # terraform -chdir=infra/oracle output public_ip
```

### Inspecting the database

The controller image has no `sqlite3` binary, so query through a throwaway container that mounts
the same volume:

```bash
docker run --rm -v evsolar_evsolar-data:/d alpine sh -c \
  "apk add --no-cache sqlite >/dev/null 2>&1; sqlite3 -header -column /d/evsolar.db \
   'SELECT * FROM vehicle_state;'"
```

Tables: `vehicle_state`, `solar_readings`, `api_usage`, `secrets`.

`solar_readings` is never pruned. A year is roughly a megabyte and it is the only real measurement
of this array — the simulations still run on an invented 4.2 kW peak. `PruneSolarReadings` exists in
the store but nothing calls it.

---

## Deploying

Code ships through GitHub Actions to GHCR, then gets pulled on the host.

```bash
# 1. branch, commit, PR, merge (never commit straight to main)
# 2. wait for the arm64 image — confirm it is for YOUR commit, not the previous one:
gh run list --workflow=build-containers.yml --limit 1 --json headSha,conclusion

# 3. on the host
ssh -i ~/.ssh/oci_evsolar ubuntu@<ip> \
  'cd /opt/evsolar && docker compose pull controller && docker compose up -d --force-recreate controller'
```

`docker compose up -d` alone will report "Running" and silently keep the old image. Use
`--force-recreate`, and check `docker image inspect ... --format '{{.Created}}'` against the build
time before believing a deploy landed.

### Terraform

```bash
cd infra/oracle
terraform plan          # ALWAYS. read it.
terraform apply         # never -auto-approve
```

**Never destroy or replace the instance.** A1 capacity is scarce — the first launch took 71
attempts. `main.tf` carries `prevent_destroy = true` plus `ignore_changes` on `metadata["user_data"]`
and the image id, because editing cloud-init otherwise forces replacement. cloud-init only runs on
first boot anyway, so apply host changes over SSH instead.

If capacity is unavailable, `infra/oracle/launch-retry.sh` cycles the availability domains. It stops
on any non-capacity error, which is deliberate: a 404 means the shape is not offered in the region
and retrying for hours would hide that.

---

## Traps that have already cost time

**ZeroMQ topics are namespaced.** fleet-telemetry publishes on `<namespace>_<recordType>`, so
`evsolar_V`, not `V`. A ZMQ SUB filter is a *prefix match*, so subscribing to `V` matches nothing and
every record is dropped **with no error logged anywhere** — fleet-telemetry reports a successful
dispatch and the controller never sees a frame. The only evidence is the `"V":"174"` counter in a
`socket_disconnected` line.

**Oracle's Ubuntu images reject on the INPUT chain.** A port opened in the VCN security list is
still refused by the instance. Both are required. `cloud-init.yaml` inserts ACCEPT rules above the
trailing REJECT on first boot; anything added later must be done by hand and persisted with
`netfilter-persistent save`.

Diagnosing which layer is blocking: **connection refused** means the packet reached the host and
nothing was listening — the firewall is open. **Timeout** means the security list dropped it.

**Bind-mounted keys need the container's uid.** The containers run non-root and the key files are
0600, so a host-owned file is unreadable inside. `controller` is **uid 10001**, `fleet-telemetry` is
**uid 65532**. `chown` the mounted files accordingly.

**The fleet-telemetry image sets no ENTRYPOINT.** Its default CMD is already
`/fleet-telemetry -config /etc/fleet-telemetry/config.json`. Any `command:` in compose is taken as
the executable itself and the container dies immediately.

**No stray keys inside `records` in the telemetry config.** Every key there must map to an array of
dispatchers; a `_comment` string fails to unmarshal and panics the server on start.

**Tesla rejects a telemetry hostname outside the partner domain.** `fleet_telemetry_config` returns
*"hostname domain does not match with partner account"* unless the hostname is under the domain the
partner account was registered with. A DuckDNS name carries one A record and DuckDNS has no CNAME,
so the public key and the telemetry endpoint must share a host — hence the nginx container.

**On-change telemetry strands `at_home`.** Signals transmit only when they change, and a parked
car's Location never changes. A drive home through cellular dead zones drops the frames that would
have said "home", freezing `at_home` on "away" while the car charges on the home connector — the
controller then skips the session entirely (observed live 2026-08-18: car 23m from home, 22A on
grid, `SkipNotAtHome` every cycle). Location is therefore registered with
`resend_interval_seconds: 600` plus `prefer_typed: true` (which resend requires). If the gate ever
sticks again, first check the registered config with `evsolar-register -status`.

**Refresh tokens rotate on every use.** Both Enphase and Tesla invalidate the previous token when
issuing a new one. The copy in SQLite is authoritative once the first refresh has happened; the
`.env` value is a **seed**, used only when the store has none. Never re-seed from a stale `.env`,
and never run a tool that refreshes a token anywhere other than where the database lives — that is
why `evsolar-register` ships inside the image.

**One horizon per question.** `LookbackWindow` (20m) is for targeting only — short, so the current
tracks a declining sun. `SustainedWindow` (45m) is what the wake gate counts over. Sharing a single
value made the wake gate unsatisfiable: pruning at the lookback horizon deleted the previous reading
moments before the gate counted it, leaving one reading where the gate needed two. Config validation
now rejects a sustained window shorter than the lookback, and nothing prunes at all. This is the
same mistake as the daylight window bounding both the call budget and the controller's authority —
watch for it.

**Enphase reading timestamps are not tick times.** They carry Enphase's `last_report_at`, roughly
two minutes earlier. A test that uses tick times for readings will pass against pruning bugs that
production hits, because the previous reading lands exactly on the boundary and survives.

**The consumption meter is fine. Do not "correct" it.** On 2026-08-20 a whole diagnosis was built
on the idea that the consumption CTs were reversed, and it was wrong. Two mistakes drove it. First,
reported consumption looked like a fixed ~0.55 of production across one sunny afternoon — but that
is a coincidence of scale plus a real confound: air conditioning works hardest when the sun is
strongest, so load genuinely correlates with production (r=+0.14 over a week of sunny intervals,
which is weak and expected). Second, the "validation" was circular: the branch of a piecewise flip
was chosen *because* it flattened the correlation with sunshine, and the flattened correlation was
then cited as proof. Shipped, it drew a house line that tracked solar at r=+0.99 — the exact
artefact it was meant to remove.

Raw reported consumption passes every independent check: overnight idle 148-214 W, morning ramp,
2080 W evening peak, 22.7 kWh/day, never negative, and it moves with the car's known draw. Its
hourly profile is a textbook household curve. A fitted coefficient (0.583) was the tell that the
model was wrong — genuine miswiring gives clean factors (x2, /2, a sign), never a magic number. If
a correction is ever proposed again, it needs an *independent* anchor: the utility bill, or the
Envoy's local per-CT readings at `/ivp/meters/readings`.

**Poll `latest_telemetry`, not `summary`.** `summary` carries production only, which made house load
look like it needed a second call and a second call is not affordable. `latest_telemetry` returns
both meters, per CT channel, on one timestamp, for the same single call. Production is the sum of
the production channels and equals `summary`'s `current_power` exactly — verified against the live
system, because charging reads that number. Null-powered channels are uninstalled CTs; never count
them.

**Enphase allows 1000 calls/month.** 27/day is ~840/month. The budget guard hard-stops at 950
*before* the request leaves the process. Failures are never retried within a run: a missed cycle is
harmless, a retry storm is not.

---

**The dashboard mirror is one-way and optional.** `internal/mirror` ships state to Supabase through
a SQLite outbox; the web app (bbarber/EvSolarDashboard, Vercel + Supabase) only reads. Nothing in
charging may ever depend on Supabase being reachable, and Record* calls must never fail the caller.

## Invariants — do not break these without a decision

- **Decisions ride on events; the clock exists for the Enphase poll.** Telemetry and connectivity
  evaluate on arrival. The first wake this system ever sent was wasted because the car slept again
  inside the tick interval — do not reintroduce a wait.
- **Session state is one enum with named transitions** (`Auto`, `StoppedForSun`, `StoppedAtCap`,
  `Overridden`), stored with the time it was entered. It replaced three nullable markers whose
  interactions caused every subtle bug in the project's history. Transitions happen in exactly two
  places: `domain.ApplyObservation` (unplug, override detection) and `controller.act` (our own
  commands). Do not add a third.

- **Never poll the vehicle.** All vehicle state arrives by push.
- **Only charge on solar.** With the sun down, a running session is stopped. Stopping short of the
  state-of-charge cap is the correct outcome, not a shortfall.
- **The daylight window bounds the Enphase poll, not the controller's authority.** The loop ticks
  around the clock. Conflating these once let a car plugged in at 04:30 reach 100% on grid power.
- **A failed poll is not sunset.** Missing data must not stop a running session; missing sun must.
- **Never send a command to a car that is not actively charging**, with two exceptions: resuming a
  session this controller stopped for low solar, and — only when `StartWhenPluggedIn` is set —
  starting a car that connectivity currently reports as online and plugged in.
- **Reachability comes from connectivity events, never from data age.** A connected, parked car
  sends nothing; it is indistinguishable from one asleep by timestamp alone.
- **Staleness means silence on BOTH channels, and a known-asleep car is not stale.** Its silence is
  explained, and charge state cannot change while asleep. Treating asleep as "unknown" made a
  Thursday-night plug-in unreachable all Friday morning: the wake path hangs off the decision, and
  staleness preempted the decision. Likewise a resume against a known-asleep car reports
  SkipNotCharging — the door to the wake gates — rather than issuing a command that fails.
- **Only the home connector is managed** when `EVSOLAR_HOME_LAT/LON` are set — no commands, not
  even stops, anywhere else, and never on a DC fast charger. The telemetry schema has no charger
  identity, so home is a position compared in-process; only the boolean `at_home` is stored. Each
  VIN is evaluated independently — never reintroduce "act on the latest reporter": a driven car reports
  constantly and eclipses the one charging at home.
- **Waking is opt-in, day-restricted, rate-limited and counted.** It costs $0.02, drains the
  battery it is meant to fill, and is the only action that disturbs a car that did not ask to be.
  `domain.DecideWake` holds every gate; do not add a caller that bypasses it.
- **An override persists until the car unplugs** — not until charging pauses or completes.
- **Record `LastSetAmps` only after a command succeeds.** Recording it for a command the car never
  received makes the next telemetry frame look like a manual override.
- **Clear `LowSolarStopIssuedAt` before commanding a resume**, or the controller mistakes its own
  resume for a person restarting the session.

---

## Status

Live and running. Telemetry, solar polling, decisions and the budget counter are all confirmed
working against real hardware.

Not yet exercised: **no charging command has ever been sent to a real vehicle.** Production has not
cleared the 5A floor since the system went live, so every decision so far has been a correct no-op.
The first real `SetChargingAmps` is still ahead, and with it the first real test of override
detection — see the open question about `ChargeAmps` vs `ChargeCurrentRequest` in
`docs/ARCHITECTURE-DECISIONS.md`.

The array's true peak output is also still unknown; the 4.2 kW figure in the simulations was
invented to make a 16A ceiling coherent and has no basis in measurement.
