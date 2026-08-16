// Package tesla sends signed charging commands straight to the vehicle.
//
// Vehicles built after 2021 reject unsigned commands — silently, as far as HTTP is concerned — so
// every command has to be signed with the application's private key using Tesla's vehicle-command
// protocol: ECDH session establishment with the car, AES-GCM encryption, and anti-replay counters.
//
// This project previously ran Tesla's tesla-http-proxy in a second container to do that signing,
// because implementing the protocol in C# would have meant roughly 800 lines of cryptographic code
// with no vehicle to test against. In Go the protocol is simply a library, so the proxy, the nginx
// wrapper that re-terminated its mandatory TLS, the shared secret guarding its public ingress, and
// the extra open port all disappear.
package tesla

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/teslamotors/vehicle-command/pkg/account"
	"github.com/teslamotors/vehicle-command/pkg/cache"
	"github.com/teslamotors/vehicle-command/pkg/protocol"
	"github.com/teslamotors/vehicle-command/pkg/vehicle"
)

// Provider is the key this client's calls are counted under.
const Provider = "tesla"

type Options struct {
	ClientID string
	TokenURL string

	// PrivateKeyPath is the EC (prime256v1) key whose public half is published at
	// /.well-known/appspecific/com.tesla.3p.public-key.pem and paired to each vehicle as a
	// virtual key. Losing it means re-pairing both cars by hand.
	PrivateKeyPath string

	// SessionCachePath persists per-vehicle session state across restarts, so a command after a
	// restart does not need a fresh handshake round-trip with the car.
	SessionCachePath string

	RefreshTokenSecretName string
	UserAgent              string
	TokenRefreshSkew       time.Duration
}

func DefaultOptions() Options {
	return Options{
		TokenURL:               "https://fleet-auth.prd.vn.cloud.tesla.com/oauth2/v3/token",
		RefreshTokenSecretName: "tesla-refresh-token",
		UserAgent:              "EvSolarChargeController/1.0",
		TokenRefreshSkew:       5 * time.Minute,
	}
}

// TokenStore persists the rotating refresh token.
type TokenStore interface {
	GetSecret(ctx context.Context, name string) (string, error)
	PutSecret(ctx context.Context, name, value string) error
}

type Commander struct {
	opts     Options
	http     *http.Client
	tokens   TokenStore
	log      *slog.Logger
	key      protocol.ECDHPrivateKey
	sessions *cache.SessionCache

	mu            sync.Mutex
	accessToken   string
	accessExpires time.Time
}

func New(opts Options, tokens TokenStore, log *slog.Logger) (*Commander, error) {
	key, err := protocol.LoadPrivateKey(opts.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("loading the command private key from %s: %w", opts.PrivateKeyPath, err)
	}

	// A missing or corrupt cache is not fatal — it only costs one extra handshake per vehicle.
	sessions, err := cache.ImportFromFile(opts.SessionCachePath)
	if err != nil {
		log.Info("starting with an empty session cache", "path", opts.SessionCachePath, "reason", err)
		sessions = cache.New(0)
	}

	return &Commander{
		opts:     opts,
		http:     &http.Client{Timeout: 30 * time.Second},
		tokens:   tokens,
		log:      log,
		key:      key,
		sessions: sessions,
	}, nil
}

// SetChargingAmps asks the vehicle to draw the given current.
func (c *Commander) SetChargingAmps(ctx context.Context, vin string, amps int) error {
	return c.withVehicle(ctx, vin, func(car *vehicle.Vehicle) error {
		return car.SetChargingAmps(ctx, int32(amps))
	})
}

// Wake brings a sleeping vehicle online so a command can reach it.
//
// Tesla bills this at $0.02 — twenty times a command, the most expensive call in the API — and it
// keeps the car awake for a while afterwards, draining the battery it is meant to fill. Callers
// gate it hard; see domain.DecideWake.
//
// Deliberately does not go through withVehicle. That opens a signed session, which is precisely
// what cannot be done with a sleeping car — it fails with "vehicle is offline or asleep" before a
// wake could ever be sent. Wakeup is a plain Fleet API call and needs no session.
func (c *Commander) Wake(ctx context.Context, vin string) error {
	token, err := c.accessTokenFor(ctx, time.Now())
	if err != nil {
		return err
	}

	acct, err := account.New(token, c.opts.UserAgent)
	if err != nil {
		return fmt.Errorf("building the Tesla account client: %w", err)
	}

	car, err := acct.GetVehicle(ctx, vin, c.key, c.sessions)
	if err != nil {
		return fmt.Errorf("resolving vehicle %s: %w", vin, err)
	}

	if err := car.Wakeup(ctx); err != nil {
		return fmt.Errorf("waking %s: %w", vin, err)
	}
	return nil
}

// StopCharging ends the session. Used at the state-of-charge cap and when solar cannot cover the
// connector minimum.
func (c *Commander) StopCharging(ctx context.Context, vin string) error {
	return c.withVehicle(ctx, vin, func(car *vehicle.Vehicle) error {
		return car.ChargeStop(ctx)
	})
}

// StartCharging resumes a session this controller stopped for low solar, then sets the current.
//
// Both happen on one connection: a resume that left the car at the previous session's amps would
// draw whatever it defaulted to until the next cycle noticed.
func (c *Commander) StartCharging(ctx context.Context, vin string, amps int) error {
	return c.withVehicle(ctx, vin, func(car *vehicle.Vehicle) error {
		if err := car.ChargeStart(ctx); err != nil {
			return err
		}
		return car.SetChargingAmps(ctx, int32(amps))
	})
}

// withVehicle opens an authenticated, signed session to one car and runs fn against it.
//
// Only the infotainment domain is started. Charging commands terminate there, and starting the
// security domain as well would mean a second handshake for no benefit.
func (c *Commander) withVehicle(ctx context.Context, vin string, fn func(*vehicle.Vehicle) error) error {
	token, err := c.accessTokenFor(ctx, time.Now())
	if err != nil {
		return err
	}

	// The Fleet API host is derived from the token's audience claim rather than configured, so a
	// token issued for the wrong region cannot silently point at the wrong host.
	acct, err := account.New(token, c.opts.UserAgent)
	if err != nil {
		return fmt.Errorf("building the Tesla account client: %w", err)
	}

	car, err := acct.GetVehicle(ctx, vin, c.key, c.sessions)
	if err != nil {
		return fmt.Errorf("resolving vehicle %s: %w", vin, err)
	}

	if err := car.Connect(ctx); err != nil {
		return fmt.Errorf("connecting to %s: %w", vin, err)
	}
	defer car.Disconnect()

	if err := car.StartSession(ctx, []protocol.Domain{protocol.DomainInfotainment}); err != nil {
		return fmt.Errorf("starting a signed session with %s: %w", vin, err)
	}

	cmdErr := fn(car)

	// Persist the session even when the command failed: the handshake still advanced, and
	// discarding it would force a fresh one next time.
	if err := car.UpdateCachedSessions(c.sessions); err != nil {
		c.log.Warn("updating the session cache", "vin", vin, "error", err)
	} else if err := c.sessions.ExportToFile(c.opts.SessionCachePath); err != nil {
		c.log.Warn("writing the session cache", "path", c.opts.SessionCachePath, "error", err)
	}

	if cmdErr != nil {
		return fmt.Errorf("command against %s: %w", vin, cmdErr)
	}
	return nil
}

func (c *Commander) accessTokenFor(ctx context.Context, now time.Time) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.accessToken != "" && now.Before(c.accessExpires.Add(-c.opts.TokenRefreshSkew)) {
		return c.accessToken, nil
	}

	refreshToken, err := c.tokens.GetSecret(ctx, c.opts.RefreshTokenSecretName)
	if err != nil {
		return "", fmt.Errorf("no Tesla refresh token available under %q: %w",
			c.opts.RefreshTokenSecretName, err)
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {c.opts.ClientID},
		"refresh_token": {refreshToken},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.opts.TokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("building the Tesla token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("tesla token refresh: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("tesla token refresh returned HTTP %d: %s",
			resp.StatusCode, truncate(string(body), 500))
	}

	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &token); err != nil {
		return "", fmt.Errorf("tesla token endpoint returned an unreadable body: %w", err)
	}
	if token.AccessToken == "" {
		return "", fmt.Errorf("tesla token endpoint returned no access_token")
	}

	lifetime := token.ExpiresIn
	if lifetime <= 0 {
		lifetime = 28800 // Tesla access tokens are 8 hours
	}
	c.accessToken = token.AccessToken
	c.accessExpires = now.Add(time.Duration(lifetime) * time.Second)

	// Tesla rotates the refresh token on use, same as Enphase.
	if token.RefreshToken != "" && token.RefreshToken != refreshToken {
		if err := c.tokens.PutSecret(ctx, c.opts.RefreshTokenSecretName, token.RefreshToken); err != nil {
			return "", fmt.Errorf("persisting the rotated Tesla refresh token: %w", err)
		}
	}

	return c.accessToken, nil
}

// KeyFingerprint is a convenience for the startup log, so a mismatched key is visible immediately
// rather than as an unexplained rejected command.
func (c *Commander) KeyFingerprint() string {
	pub := c.key.PublicBytes()
	if len(pub) < 8 {
		return "unknown"
	}
	return fmt.Sprintf("%x", pub[:8])
}

func truncate(v string, max int) string {
	if len(v) <= max {
		return v
	}
	return v[:max] + "…"
}
