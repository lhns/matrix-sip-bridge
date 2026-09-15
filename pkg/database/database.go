// Package database holds the bridge's own tables, which track in-flight calls.
//
// bridgev2 owns the main dbutil.UpgradeTable and its version row, so these
// tables live in a child database with a version table of their own. Sharing
// the parent upgrade table would mean bridgev2 upgrades and bridge upgrades
// racing for the same version number.
package database

import (
	"context"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
)

// versionTable is the name of this schema's own version row holder. It must not
// collide with bridgev2's "database_owner"/"version" tables.
const versionTable = "sip_bridge_version"

// Database is the bridge's own schema.
type Database struct {
	*dbutil.Database
	Call *CallQuery
}

// New wraps a bridgev2 database in a child that owns the sip_* tables.
//
// log reaches the schema upgrade only. Query logging cannot be redirected
// here: dbutil.Database.Child copies the parent's LoggingDB by value, and its
// internal pointer still names the parent, so every query this schema runs is
// logged with the parent's logger whatever is passed in.
func New(db *dbutil.Database, log zerolog.Logger) *Database {
	child := db.Child(versionTable, upgradeTable, dbutil.ZeroLogger(log))
	return &Database{
		Database: child,
		Call:     &CallQuery{dbutil.MakeQueryHelper(child, newCall)},
	}
}

// Upgrade applies any pending schema changes to the bridge's own tables.
func (db *Database) Upgrade(ctx context.Context) error {
	return db.Database.Upgrade(ctx)
}
