// Package enphase talks to the Enphase Enlighten API v4 on the free "Watt" plan.
//
// The plan allows 1000 calls a month. At three calls an hour across a nine-hour window a 31-day
// month costs 837, which is a thin enough margin that this client deliberately never retries: a
// missed 20-minute cycle is harmless because the next one picks up fresh data, whereas a retry
// storm can burn the month's budget in an afternoon.
package enphase

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
)

// Provider is the key this client's calls are counted under.
const Provider = "enphase"

type Options struct {
	BaseURL      string
	TokenURL     string
	ClientID     string
	ClientSecret string
	APIKey       string
	SystemID     string

	// MonthlyCallBudget is a hard stop below the plan's real cap, so a retried deploy or a
	// 31-day month cannot push usage over.
	MonthlyCallBudget int

	// TokenRefreshSkew refreshes the access token this long before it actually expires.
	TokenRefreshSkew time.Duration

	// RefreshTokenSecretName is the key the rotating refresh token is stored under.
	RefreshTokenSecretName string
}

func DefaultOptions() Options {
	return Options{
		BaseURL:                "https://api.enphaseenergy.com/api/v4",
		TokenURL:               "https://api.enphaseenergy.com/oauth/token",
		MonthlyCallBudget:      950,
		TokenRefreshSkew:       30 * time.Minute,
		RefreshTokenSecretName: "enphase-refresh-token",
	}
}

// FailureReason explains why a poll produced no reading. Callers log it and skip the cycle.
type FailureReason string

const (
	ReasonNone        FailureReason = ""
	ReasonRateLimited FailureReason = "rate_limited"
	ReasonQuotaGuard  FailureReason = "quota_guard"
	ReasonAuthFailed  FailureReason = "auth_failed"
	ReasonTransport   FailureReason = "transport_error"
	ReasonUnexpected  FailureReason = "unexpected_response"
)

// Production is a single production sample from the Enphase cloud.
type Production struct {
	Watts     float64
	ReadingAt time.Time
}

type Result struct {
	Production *Production
	Reason     FailureReason
	Message    string
}

func (r Result) Success() bool { return r.Production != nil }

func fail(reason FailureReason, format string, args ...any) Result {
	return Result{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// TokenStore persists the rotating refresh token.
type TokenStore interface {
	GetSecret(ctx context.Context, name string) (string, error)
	PutSecret(ctx context.Context, name, value string) error
}

// UsageRecorder counts outbound calls against the monthly budget.
type UsageRecorder interface {
	MonthlyCallCount(ctx context.Context, provider string, at time.Time) (int, error)
	RecordAPICall(ctx context.Context, provider string, at time.Time, failed bool) (int, error)
}

type Client struct {
	opts   Options
	http   *http.Client
	tokens TokenStore
	usage  UsageRecorder
	log    *slog.Logger

	// Access tokens live a day, so caching one in process keeps a restart-free day down to a
	// single token call rather than one per cycle.
	mu            sync.Mutex
	accessToken   string
	accessExpires time.Time
}

func New(opts Options, tokens TokenStore, usage UsageRecorder, log *slog.Logger) *Client {
	return &Client{
		opts:   opts,
		http:   &http.Client{Timeout: 30 * time.Second},
		tokens: tokens,
		usage:  usage,
		log:    log,
	}
}

// CurrentProduction reads instantaneous production. It never retries, and it refuses to spend a
// call once the configured budget is reached.
func (c *Client) CurrentProduction(ctx context.Context, now time.Time) Result {
	used, err := c.usage.MonthlyCallCount(ctx, Provider, now)
	if err != nil {
		return fail(ReasonUnexpected, "reading the call counter failed: %v", err)
	}
	if used >= c.opts.MonthlyCallBudget {
		c.log.Error("enphase monthly call budget exhausted",
			"used", used, "budget", c.opts.MonthlyCallBudget)
		return fail(ReasonQuotaGuard,
			"Enphase monthly call budget exhausted (%d/%d); skipping poll.", used, c.opts.MonthlyCallBudget)
	}

	accessToken, err := c.accessTokenFor(ctx, now)
	if err != nil {
		c.log.Error("enphase authentication failed", "error", err)
		return fail(ReasonAuthFailed, "%v", err)
	}

	endpoint := fmt.Sprintf("%s/systems/%s/summary?key=%s",
		strings.TrimRight(c.opts.BaseURL, "/"), c.opts.SystemID, url.QueryEscape(c.opts.APIKey))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fail(ReasonUnexpected, "building request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := c.http.Do(req)
	if err != nil {
		// The call left the process, so it counts against the budget even though it failed.
		if _, recErr := c.usage.RecordAPICall(ctx, Provider, now, true); recErr != nil {
			c.log.Warn("recording a failed enphase call", "error", recErr)
		}
		c.log.Warn("enphase production request failed at the transport layer", "error", err)
		return fail(ReasonTransport, "%v", err)
	}
	defer resp.Body.Close()

	succeeded := resp.StatusCode >= 200 && resp.StatusCode < 300
	total, err := c.usage.RecordAPICall(ctx, Provider, now, !succeeded)
	if err != nil {
		c.log.Warn("recording an enphase call", "error", err)
	}
	c.log.Info("enphase call complete",
		"count", total, "budget", c.opts.MonthlyCallBudget, "status", resp.StatusCode)

	if resp.StatusCode == http.StatusTooManyRequests {
		return fail(ReasonRateLimited, "Enphase returned 429; skipping this cycle without retrying.")
	}
	if !succeeded {
		return fail(ReasonUnexpected, "Enphase returned HTTP %d: %s", resp.StatusCode, truncate(readBody(resp.Body), 500))
	}

	var payload struct {
		CurrentPower *float64 `json:"current_power"`
		LastReportAt *int64   `json:"last_report_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return fail(ReasonUnexpected, "Enphase summary response was unreadable: %v", err)
	}
	if payload.CurrentPower == nil {
		return fail(ReasonUnexpected, "Enphase summary response did not include current_power.")
	}

	readingAt := now
	if payload.LastReportAt != nil {
		readingAt = time.Unix(*payload.LastReportAt, 0).UTC()
	}

	return Result{Production: &Production{Watts: *payload.CurrentPower, ReadingAt: readingAt}}
}

func (c *Client) accessTokenFor(ctx context.Context, now time.Time) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.accessToken != "" && now.Before(c.accessExpires.Add(-c.opts.TokenRefreshSkew)) {
		return c.accessToken, nil
	}

	refreshToken, err := c.tokens.GetSecret(ctx, c.opts.RefreshTokenSecretName)
	if err != nil {
		return "", fmt.Errorf("no Enphase refresh token available under %q: %w",
			c.opts.RefreshTokenSecretName, err)
	}

	endpoint := fmt.Sprintf("%s?grant_type=refresh_token&refresh_token=%s",
		c.opts.TokenURL, url.QueryEscape(refreshToken))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(""))
	if err != nil {
		return "", fmt.Errorf("building token request: %w", err)
	}
	req.SetBasicAuth(c.opts.ClientID, c.opts.ClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("enphase token refresh: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf(
			"enphase token refresh returned HTTP %d: %s. Refresh tokens expire after one month — "+
				"a full re-authorization may be required", resp.StatusCode, truncate(readBody(resp.Body), 500))
	}

	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil {
		return "", fmt.Errorf("enphase token endpoint returned an unreadable body: %w", err)
	}
	if token.AccessToken == "" {
		return "", fmt.Errorf("enphase token endpoint returned no access_token")
	}

	lifetime := token.ExpiresIn
	if lifetime <= 0 {
		lifetime = 86400
	}
	c.accessToken = token.AccessToken
	c.accessExpires = now.Add(time.Duration(lifetime) * time.Second)

	// Enphase rotates the refresh token on every use. Persisting it is not optional: miss this
	// and the next process start is locked out until someone re-authorizes by hand.
	if token.RefreshToken != "" && token.RefreshToken != refreshToken {
		if err := c.tokens.PutSecret(ctx, c.opts.RefreshTokenSecretName, token.RefreshToken); err != nil {
			return "", fmt.Errorf("persisting the rotated refresh token: %w", err)
		}
	}

	return c.accessToken, nil
}

func readBody(r io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(r, 4096))
	if err != nil {
		return "<unreadable>"
	}
	return string(b)
}

func truncate(v string, max int) string {
	if len(v) <= max {
		return v
	}
	return v[:max] + "…"
}
