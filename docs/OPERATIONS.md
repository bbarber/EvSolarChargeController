# Operations

How the running system looks after itself, the few things that can still need a person, and what
to do when they do. [SETUP.md](SETUP.md) builds the system from nothing; this keeps it running.

**Routine maintenance: none.** Nothing here has a calendar task. Everything below is either
automatic or a response to a specific symptom or email.

---

## What runs unattended

| What | Where | How often | Evidence it is working |
|---|---|---|---|
| Telemetry certificate renewal | VM: `evsolar-cert-renew.timer` (`deploy/cert-renewal/`) | daily; renews at ~30 days left | `sudo journalctl -u evsolar-cert-renew` |
| Certificate expiry alarm | GitHub: **Check telemetry certificate** | daily 06:41 UTC | green runs; a failure **emails you** |
| Tesla + Enphase token refresh | controller, rotated into SQLite | on use | the controller keeps logging decisions |
| Enphase call budget | controller, hard stop at 950/month | each poll | `api_usage` table |
| Container restarts | Docker `restart: unless-stopped` | on crash or reboot | `docker compose ps` |

### Certificate renewal in detail

fleet-telemetry terminates mutual TLS itself, so no managed certificate service can sit in front of
it. The certificate is a Let's Encrypt one issued over DNS-01 through DuckDNS, because nothing
answers HTTP on the hostname.

- `/opt/evsolar/cert-renewal/renew-cert.sh` builds a pinned certbot image (cached), runs
  `certbot renew`, and when the certificate changes copies it into `/opt/evsolar/fleet-telemetry/`
  (owned by uid 65532) and restarts **only** `fleet-telemetry` and `public-key`. The controller
  keeps running.
- It then reads the certificate actually being served on 8443 and fails the unit if it has under
  14 days left.
- The DuckDNS token lives in certbot's renewal config,
  `/opt/evsolar/letsencrypt/config/renewal/telemetry.conf` (directory is root-only). **No
  credential for this system is stored in GitHub.**
- The GitHub check reads both ports from the internet and fails when under a quarter of the
  certificate's lifetime remains. Renewal happens at about a third, so a failure means renewal has
  been broken for roughly a week, with about three weeks still left. It re-enables its own schedule
  on every run, because GitHub otherwise disables scheduled workflows after 60 days without repo
  activity. That is how the original renewal job died unnoticed and let the certificate expire on
  2026-11-14.

---

## When something needs you

### You got a "Check telemetry certificate" failure email

Renewal on the VM has been failing for about a week. You have roughly three weeks.

```bash
ssh -i ~/.ssh/oci_evsolar ubuntu@<ip>          # terraform -chdir=infra/oracle output public_ip
sudo journalctl -u evsolar-cert-renew -n 50 --no-pager
sudo systemctl start evsolar-cert-renew         # retry by hand; the log says what failed
```

Likely causes:
- **DuckDNS token regenerated or wrong:** the DNS challenge fails. Re-issue as in
  [SETUP step 9](SETUP.md#9--issue-the-telemetry-certificate) with the new token, which also
  updates the stored copy.
- **Let's Encrypt or PyPI briefly down:** just wait; the timer retries daily.
- **The timer is not running:** `systemctl list-timers evsolar-cert-renew.timer`. If it is
  missing, run `sudo /opt/evsolar/cert-renewal/install.sh`.
- **Check fails but the VM says the certificate is fine:** suspect DNS. The DuckDNS A record must
  point at the VM's IP.

If the check *itself* fails to run (not an expiry finding), see "GitHub workflow disabled" below.

### Telemetry stopped arriving

Symptom: the controller logs decisions from stale state, and nothing new appears in
`vehicle_state`.

```bash
cd /opt/evsolar
docker compose logs --since 1h fleet-telemetry | grep -E "socket_connected|bad certificate" | tail
echo | openssl s_client -connect localhost:8443 2>/dev/null | openssl x509 -noout -enddate
```

- **`remote error: tls: bad certificate` from the same IP every ~20s:** the car is rejecting our
  certificate. If it has expired, renew it (`sudo systemctl start evsolar-cert-renew`, or force a
  reissue per SETUP step 9). If it is valid, Let's Encrypt has probably moved issuance to a root
  the cars were not given. Re-register with the current chain, as in
  [SETUP step 11](SETUP.md#11--register-telemetry-on-each-vehicle), using
  `-ca /opt/evsolar/fleet-telemetry/server-cert.pem`. Today the registered chain reaches ISRG
  Root YE (valid to 2032), so any new Root-YE certificate is accepted without this.
- **No handshake attempts at all:** check the firewall layers (see Troubleshooting in SETUP.md),
  and `evsolar-register -status` to confirm the cars still hold our config.
- **Connections arrive but the controller sees nothing:** see the ZeroMQ topic trap in CLAUDE.md.

A car that is asleep or unplugged sends nothing, which is normal. One car connecting is enough to
prove the path works.

### The VM was down for a long time

| Down for | Consequence | Fix |
|---|---|---|
| a few days | none; the certificate has 30+ days of slack | — |
| over ~1 month | Enphase refresh token expired | redo [SETUP step 6](SETUP.md#6--enphase--dedicated-application), seed `.env`, restart controller |
| over ~3 months | Tesla refresh token likely expired too | redo [SETUP step 5](SETUP.md#5--tesla-oauth--get-a-refresh-token) |
| past certificate expiry | cars refuse telemetry | `sudo systemctl start evsolar-cert-renew` |

The `.env` refresh tokens are seeds only. SQLite holds the live copies; see CLAUDE.md.

### GitHub workflow disabled

`gh workflow list --all` shows `disabled_inactivity`. The check re-enables itself on each run, so
this should not happen, but if it does:

```bash
gh workflow enable check-telemetry-cert.yml
gh workflow run check-telemetry-cert.yml
```

This only switches off the *alarm*. Renewal on the VM does not depend on GitHub.

### A plugged-in car never started charging

A car that falls asleep before the sun is strong enough can only be charged if the controller wakes
it. Look for the `not waking` line beside each `SkipNotCharging` decision:

```bash
docker compose logs --since 12h controller | grep '"not waking"' | tail
```

Its `reason` names the wake rule that failed. The common ones:
- **Not a permitted wake day:** `EVSOLAR_WAKE_DAYS`, or the per-car
  `EVSOLAR_WAKE_DAYS_<VIN>`, in `/opt/evsolar/.env`. As deployed: Fri–Sun, and weekends only for the Model Y.
- **Daily limit or cooldown:** `EVSOLAR_MAX_WAKES_PER_DAY` and `EVSOLAR_WAKE_COOLDOWN`.
- **Within the SoC headroom of the cap:** not worth a wake.
- **Not enough sustained solar.**

Editing `.env` takes effect on
`docker compose up -d --force-recreate controller`. Each wake costs about $0.02 and some battery,
which is why it is restricted at all.

### A car is stuck in `SkipOverrideActive`

Overrides clear only on unplug. If the controller missed an unplug (for example during a telemetry
outage), unplug and replug the car once.

### The VM was rebuilt

cloud-init does **not** install certificate renewal. After the steps in SETUP.md, repeat step 9 in
full. That issues the first certificate, stores the DuckDNS token, and enables the timer. If the
IP changed, update the DuckDNS A record for `evsolarchargecontroller.duckdns.org` first.

---

## Occasional upgrades (optional, never urgent)

- **certbot:** pinned in `deploy/cert-renewal/Dockerfile`. To bump it, edit the file, copy the
  directory to `/opt/evsolar/cert-renewal/` (owned by root), and run
  `sudo systemctl start evsolar-cert-renew` to confirm it works.
- **fleet-telemetry:** pinned in `deploy/docker-compose.yml`.
- **Controller:** deploy as in CLAUDE.md (`docker compose pull controller && docker compose up -d
  --force-recreate controller`).

---

## Inventory

### GitHub configuration

| Kind | Name | Scope | Used by |
|---|---|---|---|
| Variable | `TELEMETRY_HOSTNAME` | `production` environment | Check telemetry certificate |
| Secrets | *(none)* | — | — |

All Azure-era secrets and variables were removed on 2026-11-22. Do not add a credential for the VM
to GitHub; renewal was moved onto the VM precisely so none is needed.

### Workflows

| Workflow | Purpose |
|---|---|
| CI | build, vet, test |
| Build container | arm64 controller image to GHCR |
| Check telemetry certificate | outside-in expiry alarm, described above |

### Where credentials live (never in git)

| Credential | Location |
|---|---|
| DuckDNS token | `.secrets/duckdns.env` locally; certbot renewal config on the VM |
| Tesla / Enphase app credentials | `.secrets/*.env` locally; `/opt/evsolar/.env` on the VM |
| Live refresh tokens | SQLite in the `evsolar_evsolar-data` volume |
| Command signing key | `/opt/evsolar/secrets/fleet-key.pem` (pairing it again means visiting each car) |
| TLS certificate + key | `/opt/evsolar/fleet-telemetry/`, source of truth in `/opt/evsolar/letsencrypt/` |
| SSH key to the VM | `~/.ssh/oci_evsolar` |
| OCI API key | `~/.oci/` |

### Removed on 2026-11-22

All Azure-era secrets and variables, the GitHub Pages site and its workflow (nginx on the VM serves
the public key; the repo copy is `deploy/public-key/site/`), and the old renewal workflow. The old
`evsolarchargecontroller-tel.duckdns.org` name had its address cleared. DuckDNS offers no API to
delete a name, so it still exists but resolves to nothing.
