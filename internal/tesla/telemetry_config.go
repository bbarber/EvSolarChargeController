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

// TelemetryFields is the subscription this controller needs.
//
// Intervals are a floor, not a cadence: fleet-telemetry transmits on change and no more often than
// this, so a parked car sends nothing at all.
var TelemetryFields = map[string]int{
	"DetailedChargeState":     60,  // authoritative charge state; ChargeState is deprecated
	"ChargeAmps":              60,  // what override detection compares against
	"ChargeCurrentRequest":    60,  // fallback for ChargeAmps
	"ChargeCurrentRequestMax": 300, // the car's own ceiling, which caps our targets
	"ChargePortLatch":         60,  // second unplug signal, clears an override
	"BatteryLevel":            300, // drives the state-of-charge cap
	"Soc":                     300, // some firmware reports SoC here instead
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
	for name, interval := range TelemetryFields {
		fields[name] = map[string]any{"interval_seconds": interval}
	}

	// "aud" and "iss" are overwritten by the signer, so they are deliberately not set here.
	config := jwt.MapClaims{
		"hostname": hostname,
		"port":     port,
		"ca":       caCertPEM,
		"fields":   fields,
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
