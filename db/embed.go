// Package db exposes the Atlas-managed migration directory as an embedded
// Go fs.FS. It is consumed by `dstream migrate` to apply migrations at
// runtime via the Atlas Go SDK without requiring the `atlas` CLI binary.
//
// The embed directive lives next to db/migrations so the relative path is
// trivially valid; the SQL files themselves are still authored/owned by
// the Atlas workflow (`atlas migrate diff`).
package db

import (
	"embed"
	"io/fs"

	"ariga.io/atlas/sql/migrate"
)

// MigrationsFS embeds every SQL file and the atlas.sum checksum from
// db/migrations.
//
//go:embed all:migrations
var MigrationsFS embed.FS

// MigrationsDir returns an in-memory migrate.Dir populated from MigrationsFS.
// We copy files into Atlas' MemDir so the returned value fully implements
// the Dir interface (Files, Checksum, etc.) with semantics identical to a
// real on-disk Atlas migration directory.
func MigrationsDir() (migrate.Dir, error) {
	sub, err := fs.Sub(MigrationsFS, "migrations")
	if err != nil {
		return nil, err
	}
	md := &migrate.MemDir{}
	err = fs.WalkDir(sub, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		b, readErr := fs.ReadFile(sub, path)
		if readErr != nil {
			return readErr
		}
		return md.WriteFile(path, b)
	})
	if err != nil {
		return nil, err
	}
	return md, nil
}
