package tesla

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/teslamotors/vehicle-command/pkg/account"
	"github.com/teslamotors/vehicle-command/pkg/sign"
)

// FieldConfig is one field's transmission settings in the vehicle-side telemetry configuration.
type FieldConfig struct {
	// IntervalSeconds is a floor, not a cadence: the vehicle transmits on change and no more
	// often than this, so a parked car sends nothing at all.
	IntervalSeconds int
	// ResendIntervalSeconds, when non-zero, re-sends the current value at this cadence even if
	// unchanged (firmware 2024.44.32+). Zero means on-change only.
	ResendIntervalSeconds int
}

// TelemetryFields is the subscription this controller needs.
//
// Location carries a resend interval because it is the one signal whose silence is load-bearing:
// a parked car's position never changes, so without periodic resend the at_home gate is stuck on
// whatever frame happened to arrive last — and a drive home through cellular dead zones can end
// with that frame saying "away" while the car sits on the home connector. Every other field either
// changes while it matters (amps, battery level while charging) or is healed by the reconnect
// snapshot (charge state).
var TelemetryFields = map[string]FieldConfig{
	"DetailedChargeState":     {IntervalSeconds: 60},                              // authoritative charge state; ChargeState is deprecated
	"ChargeAmps":              {IntervalSeconds: 60},                              // what override detection compares against
	"ChargeCurrentRequest":    {IntervalSeconds: 60},                              // fallback for ChargeAmps
	"ChargeCurrentRequestMax": {IntervalSeconds: 300},                             // the car's own ceiling, which caps our targets
	"ChargePortLatch":         {IntervalSeconds: 60},                              // second unplug signal, clears an override
	"BatteryLevel":            {IntervalSeconds: 300},                             // drives the state-of-charge cap
	"Soc":                     {IntervalSeconds: 300},                             // some firmware reports SoC here instead
	"Location":                {IntervalSeconds: 600, ResendIntervalSeconds: 600}, // the at-home gate; coarse on purpose, never stored raw
	"FastChargerPresent":      {IntervalSeconds: 60},                              // a DC session is never touched, wherever it is
}

// RegisterTelemetry points the given vehicles at a fleet-telemetry server.
//
// The configuration is not an ordinary API call: it has to be signed as a JWT with the application's
// command key, because the *vehicle* verifies it against the public key published at
// /.well-known/appspecific/com.tesla.3p.public-key.pem before accepting a new destination. Signing
// it here is what makes tesla-http-proxy unnecessary for setup as well as for commands.
//
// caCertPEM must be the full chain for the server's certificate.
func (c *Commander) RegisterTelemetry(ctx context.Context, vins []string, hostname string, port int, caCertPEM string) ([]byte, error) {
	token, err := c.accessTokenFor(ctx, time.Now())
	if err != nil {
		return nil, err
	}

	acct, err := account.New(token, c.opts.UserAgent)
	if err != nil {
		return nil, fmt.Errorf("building the Tesla account client: %w", err)
	}

	fields := make(map[string]any, len(TelemetryFields))
	for name, fc := range TelemetryFields {
		entry := map[string]any{"interval_seconds": fc.IntervalSeconds}
		if fc.ResendIntervalSeconds > 0 {
			entry["resend_interval_seconds"] = fc.ResendIntervalSeconds
		}
		fields[name] = entry
	}

	// "aud" and "iss" are overwritten by the signer, so they are deliberately not set here.
	// prefer_typed is required for resend_interval_seconds to take effect; the decoder has always
	// accepted the typed variants (and falls back to strings for numerics), so it is safe to pin.
	config := jwt.MapClaims{
		"hostname":     hostname,
		"port":         port,
		"ca":           caCertPEM,
		"fields":       fields,
		"prefer_typed": true,
	}

	signed, err := sign.SignMessageForFleet(c.key, "TelemetryClient", config)
	if err != nil {
		return nil, fmt.Errorf("signing the telemetry configuration: %w", err)
	}

	body, err := json.Marshal(map[string]any{"vins": vins, "token": signed})
	if err != nil {
		return nil, fmt.Errorf("serialising the registration request: %w", err)
	}

	endpoint := fmt.Sprintf("https://%s/api/1/vehicles/fleet_telemetry_config_jws", acct.Host)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building the registration request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.opts.UserAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registering telemetry: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16384))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return respBody, fmt.Errorf("telemetry registration returned HTTP %d: %s",
			resp.StatusCode, truncate(string(respBody), 800))
	}
	return respBody, nil
}

// TelemetryStatus reads back the current configuration, so the caller can wait for "synced".
func (c *Commander) TelemetryStatus(ctx context.Context, vin string) ([]byte, error) {
	token, err := c.accessTokenFor(ctx, time.Now())
	if err != nil {
		return nil, err
	}

	acct, err := account.New(token, c.opts.UserAgent)
	if err != nil {
		return nil, fmt.Errorf("building the Tesla account client: %w", err)
	}

	endpoint := fmt.Sprintf("https://%s/api/1/vehicles/%s/fleet_telemetry_config", acct.Host, vin)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("building the status request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", c.opts.UserAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reading telemetry status: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16384))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, fmt.Errorf("telemetry status returned HTTP %d: %s",
			resp.StatusCode, truncate(string(body), 800))
	}
	return body, nil
}
