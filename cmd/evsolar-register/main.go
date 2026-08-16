// Command evsolar-register points vehicles at the fleet-telemetry server, and reads back whether
// they have accepted the configuration.
//
// This exists because the configuration has to be signed with the application's command key — the
// vehicle verifies it against the published public key before accepting a new destination. Tesla's
// reference answer is to run tesla-http-proxy and POST through it; signing it here means the proxy
// is not needed for setup either.
//
//	evsolar-register -host tel.duckdns.org -port 8443 -ca /path/fullchain.pem -vins VIN1,VIN2
//	evsolar-register -status -vins VIN1
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/bbarber/EvSolarChargeController/internal/store"
	"github.com/bbarber/EvSolarChargeController/internal/tesla"
)

func main() {
	var (
		host     = flag.String("host", "", "telemetry hostname registered in DNS")
		port     = flag.Int("port", 8443, "telemetry port")
		caPath   = flag.String("ca", "", "path to the full certificate chain (fullchain.pem)")
		vinsCSV  = flag.String("vins", "", "comma-separated VINs")
		dbPath   = flag.String("db", "/var/lib/evsolar/evsolar.db", "database holding the refresh token")
		keyPath  = flag.String("key", "/etc/evsolar/fleet-key.pem", "command private key")
		clientID = flag.String("client-id", os.Getenv("EVSOLAR_TESLA_CLIENT_ID"), "Tesla application client id")
		status   = flag.Bool("status", false, "read the current configuration instead of setting it")
	)
	flag.Parse()

	if err := run(*host, *port, *caPath, *vinsCSV, *dbPath, *keyPath, *clientID, *status); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run(host string, port int, caPath, vinsCSV, dbPath, keyPath, clientID string, status bool) error {
	var vins []string
	for _, v := range strings.Split(vinsCSV, ",") {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			vins = append(vins, trimmed)
		}
	}
	if len(vins) == 0 {
		return fmt.Errorf("-vins is required")
	}
	if clientID == "" {
		return fmt.Errorf("-client-id (or EVSOLAR_TESLA_CLIENT_ID) is required")
	}

	db, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	opts := tesla.DefaultOptions()
	opts.ClientID = clientID
	opts.PrivateKeyPath = keyPath
	// Registration is a one-off; a session cache next to the database would be noise.
	opts.SessionCachePath = os.DevNull

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	commander, err := tesla.New(opts, db, log)
	if err != nil {
		return err
	}

	ctx := context.Background()

	if status {
		for _, vin := range vins {
			body, err := commander.TelemetryStatus(ctx, vin)
			if err != nil {
				return err
			}
			fmt.Printf("%s: %s\n", vin, body)
		}
		return nil
	}

	if host == "" || caPath == "" {
		return fmt.Errorf("-host and -ca are required unless -status is set")
	}
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", caPath, err)
	}

	body, err := commander.RegisterTelemetry(ctx, vins, host, port, string(ca))
	if err != nil {
		return err
	}

	fmt.Printf("registered %d vehicle(s) against %s:%d\n%s\n", len(vins), host, port, body)
	fmt.Println("\nPoll with -status until \"synced\" is true; diagnose with the fleet_telemetry_errors endpoint.")
	return nil
}
