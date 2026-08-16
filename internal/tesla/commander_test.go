package tesla

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bbarber/EvSolarChargeController/internal/store"
)

var testNow = time.Date(2026, 8, 11, 14, 0, 0, 0, time.UTC)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// writeTestKey emits a prime256v1 key in the same PEM form as
// `openssl ecparam -name prime256v1 -genkey -noout`, which is how the real key was generated.
func writeTestKey(t *testing.T, dir string) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshalling key: %v", err)
	}

	path := filepath.Join(dir, "fleet-key.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("writing key: %v", err)
	}
	return path
}

func newHarness(t *testing.T, handler http.HandlerFunc) (*Commander, *store.Store) {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if err := st.PutSecret(context.Background(), "tesla-refresh-token", "seed-refresh"); err != nil {
		t.Fatalf("seeding refresh token: %v", err)
	}

	opts := DefaultOptions()
	opts.TokenURL = srv.URL + "/oauth2/v3/token"
	opts.ClientID = "client-id"
	opts.PrivateKeyPath = writeTestKey(t, dir)
	opts.SessionCachePath = filepath.Join(dir, "sessions.json")

	c, err := New(opts, st, quietLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, st
}

func TestRefreshSendsTheDocumentedForm(t *testing.T) {
	// Tesla's refresh grant takes client_id and refresh_token — and notably no client_secret.
	c, _ := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if got := r.Form.Get("grant_type"); got != "refresh_token" {
			t.Errorf("grant_type = %q, want refresh_token", got)
		}
		if got := r.Form.Get("client_id"); got != "client-id" {
			t.Errorf("client_id = %q, want client-id", got)
		}
		if got := r.Form.Get("refresh_token"); got != "seed-refresh" {
			t.Errorf("refresh_token = %q, want seed-refresh", got)
		}
		if got := r.Form.Get("client_secret"); got != "" {
			t.Errorf("client_secret = %q, want it absent", got)
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "expires_in": 28800})
	})

	got, err := c.accessTokenFor(context.Background(), testNow)
	if err != nil {
		t.Fatalf("accessTokenFor: %v", err)
	}
	if got != "at" {
		t.Errorf("access token = %q, want at", got)
	}
}

func TestPersistsTheRotatedRefreshToken(t *testing.T) {
	c, st := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "refresh_token": "rotated", "expires_in": 28800,
		})
	})

	if _, err := c.accessTokenFor(context.Background(), testNow); err != nil {
		t.Fatalf("accessTokenFor: %v", err)
	}

	got, err := st.GetSecret(context.Background(), "tesla-refresh-token")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if got != "rotated" {
		t.Errorf("stored refresh token = %q, want rotated", got)
	}
}

func TestCachesTheAccessTokenUntilTheSkew(t *testing.T) {
	calls := 0
	c, _ := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "expires_in": 3600})
	})

	ctx := context.Background()
	c.accessTokenFor(ctx, testNow)
	c.accessTokenFor(ctx, testNow.Add(30*time.Minute)) // still outside the 5-minute skew
	if calls != 1 {
		t.Errorf("token calls = %d, want 1", calls)
	}

	c.accessTokenFor(ctx, testNow.Add(58*time.Minute)) // inside the skew
	if calls != 2 {
		t.Errorf("token calls = %d, want 2", calls)
	}
}

func TestRefreshFailureIsReportedWithTheBody(t *testing.T) {
	c, _ := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"login_required"}`))
	})

	_, err := c.accessTokenFor(context.Background(), testNow)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !contains(err.Error(), "401") || !contains(err.Error(), "login_required") {
		t.Errorf("error = %v, want it to carry the status and body", err)
	}
}

func TestMissingRefreshTokenFailsClearly(t *testing.T) {
	c, _ := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	c.opts.RefreshTokenSecretName = "never-seeded"

	_, err := c.accessTokenFor(context.Background(), testNow)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !contains(err.Error(), "never-seeded") {
		t.Errorf("error = %v, want it to name the missing secret", err)
	}
}

func TestAnEmptyAccessTokenIsRejected(t *testing.T) {
	// A 200 with no token would otherwise cache an empty string and fail every later command.
	c, _ := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"expires_in": 28800})
	})

	if _, err := c.accessTokenFor(context.Background(), testNow); err == nil {
		t.Error("expected an error when access_token is absent")
	}
}

func TestKeyFingerprintIsStable(t *testing.T) {
	c, _ := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	first := c.KeyFingerprint()
	if first == "" || first == "unknown" {
		t.Fatalf("fingerprint = %q, want a real value", first)
	}
	if second := c.KeyFingerprint(); second != first {
		t.Errorf("fingerprint changed between calls: %q then %q", first, second)
	}
}

func TestNewFailsOnAMissingKey(t *testing.T) {
	opts := DefaultOptions()
	opts.PrivateKeyPath = filepath.Join(t.TempDir(), "absent.pem")

	if _, err := New(opts, nil, quietLogger()); err == nil {
		t.Error("expected New to fail when the command key is missing")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
