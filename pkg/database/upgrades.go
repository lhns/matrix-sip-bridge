package database

import (
	"embed"
	"io/fs"

	"go.mau.fi/util/dbutil"
)

//go:embed upgrades/*.sql
var rawUpgrades embed.FS

// upgradeTable holds the schema history of the bridge's own tables.
//
// It is deliberately separate from bridgev2's UpgradeTable: bridgev2 owns the
// main one and its version row, so a child database with its own version table
// is the only way to add tables without fighting it. See Database.
var upgradeTable = dbutil.BuildUpgradeTable().WithFS(upgradeFS()).Finish()

// upgradeFS re-roots the embedded files at the directory holding them.
//
// dbutil's WithFSPath joins the directory with filepath.Join, which produces a
// backslash on Windows and then fails to find anything in an embed.FS. Rooting
// the FS first sidesteps that, so tests run on Windows as well as in CI.
func upgradeFS() interface {
	fs.ReadFileFS
	fs.ReadDirFS
} {
	sub, err := fs.Sub(rawUpgrades, "upgrades")
	if err != nil {
		panic(err)
	}
	return sub.(interface {
		fs.ReadFileFS
		fs.ReadDirFS
	})
}
