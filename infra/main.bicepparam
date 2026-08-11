using 'main.bicep'

// Environment-specific, non-secret values only. Secret values are seeded straight into Key Vault
// by tools/seed-keyvault.sh — never add a client secret or refresh token to this file.

param environmentName = 'prod'
param location = 'centralus'
param namePrefix = 'evsolar'

param timeZone = 'America/Chicago'

// 240V US residential split-phase. Confirm against the actual service before trusting the
// watts -> amps conversion.
param systemVoltage = 240
param minChargeAmps = 5

// Matches the array's peak output; requesting more would only pull the difference from the grid.
param maxChargeAmps = 16

param pollingStartHourLocal = 9
param pollingEndHourLocal = 19

// 3 calls/hour x 10 hours x 31 days = 930. The Watt plan allows 1000/month.
param enphaseMonthlyCallBudget = 950

param teslaFleetApiBaseUrl = 'https://fleet-api.prd.na.vn.cloud.tesla.com'
param teslaCommandMode = 'Proxy'

// Add both cars' VINs once known; the shared connector means only one charges at a time.
param vins = []

// Flip to true once the container images have been built and pushed by the CI workflow, and the
// certificate/config share has been seeded.
param deployContainerApps = false
param bridgeImage = ''
param teslaProxyImage = ''

// Free DuckDNS hostname. Its A record points at the telemetryStaticIp deployment output, and the
// "Renew telemetry certificate" workflow issues a Let's Encrypt certificate for it over DNS-01.
param telemetryHostname = ''

// Not 443: externally exposed TCP ports must be unique across the Container Apps environment, and
// the command proxy's HTTP ingress already holds 80/443. Register this port in fleet_telemetry_config.
param telemetryPort = 8443

// APIM is off and expected to stay off. It was originally intended to terminate mTLS for telemetry,
// which turned out not to be possible (see docs/ARCHITECTURE-DECISIONS.md), and the only remaining
// job — serving one static public key — is handled free by GitHub Pages.
param deployApim = false
param apimPublisherEmail = ''
param apimCustomDomainName = ''
param apimCustomDomainCertificateSecretId = ''
param teslaPublicKeyPem = ''
