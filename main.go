// Command matrix-sip-bridge bridges Matrix to a SIP network as an endpoint of
// it: text as SIP MESSAGE, and calls as real media via LiveKit.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"

	"github.com/lhns/matrix-sip-bridge/pkg/connector"
)

// Version is the latest release. mxmain compares it with the git tag baked in
// at build time to tell a release build from a dev one.
var Version = "0.1.0"

// Tag, Commit and BuildTime are filled in by the linker; see the Makefile.
var (
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// Backoff for the first database connection. dbutil.NewFromConfig only opens a
// pool; the first dial is br.DB.Upgrade inside Start, where a failure exits
// immediately and mxmain retries nothing.
const (
	dbPingBackoff  = 250 * time.Millisecond
	dbPingMaxDelay = 5 * time.Second
	dbPingDeadline = 60 * time.Second
)

func main() {
	m := mxmain.BridgeMain{
		Name:        "matrix-sip-bridge",
		Description: "A Matrix bridge that is a SIP endpoint, for messages and calls",
		URL:         "https://github.com/lhns/matrix-sip-bridge",
		Version:     Version,
		Connector:   &connector.SIPConnector{},
	}
	// PostInit runs at the end of mxmain's Init, after the pool exists and
	// before Start dials it: the only place a retry can be inserted.
	m.PostInit = func() {
		err := waitForDB(context.Background(), m.DB.RawDB.PingContext, dbPingBackoff, dbPingMaxDelay, dbPingDeadline, m.Log)
		if err != nil {
			m.Log.WithLevel(zerolog.FatalLevel).Err(err).Msg("Failed to connect to the database")
			// The exit code mxmain itself uses for a database it cannot open.
			os.Exit(14)
		}
	}
	m.InitVersion(Tag, Commit, BuildTime)
	m.Run()
}

// waitForDB pings until the database answers, doubling the delay from backoff
// to maxDelay and giving up after deadline.
//
// A pod usually starts before its network policy is programmed, and a vnet that
// rejects rather than drops makes that first dial fail as "connection refused",
// which reads as permanent. A Postgres failover looks the same.
func waitForDB(
	ctx context.Context,
	ping func(context.Context) error,
	backoff, maxDelay, deadline time.Duration,
	log *zerolog.Logger,
) error {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	delay := backoff
	for attempt := 1; ; attempt++ {
		err := ping(ctx)
		if err == nil {
			if attempt > 1 {
				log.Info().Int("attempts", attempt).Msg("Database is reachable")
			}
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("database unreachable after %s: %w", deadline, err)
		}
		log.Warn().Err(err).Int("attempt", attempt).Dur("retry_in", delay).Msg("Database is not reachable yet")
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("database unreachable after %s: %w", deadline, err)
		case <-timer.C:
		}
		delay = min(delay*2, maxDelay)
	}
}
