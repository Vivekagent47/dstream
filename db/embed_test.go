package db

import (
	"os"
	"path/filepath"
	"testing"

	"ariga.io/atlas/sql/migrate"
)

// The embedded directory must be exactly what is on disk (so a binary cannot
// ship stale migrations) and must carry a checksum atlas accepts.
func TestMigrationsDirMatchesDisk(t *testing.T) {
	dir, err := MigrationsDir()
	if err != nil {
		t.Fatalf("MigrationsDir: %v", err)
	}
	onDisk, err := filepath.Glob("migrations/*.sql")
	if err != nil || len(onDisk) == 0 {
		t.Fatalf("no migrations on disk (%v)", err)
	}
	files, err := dir.Files()
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(files) != len(onDisk) {
		t.Fatalf("embedded %d sql files, disk has %d", len(files), len(onDisk))
	}
	for _, p := range onDisk {
		want, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		f, err := dir.Open(filepath.Base(p))
		if err != nil {
			t.Fatalf("embedded dir lacks %s: %v", p, err)
		}
		got := make([]byte, len(want)+1)
		n, _ := f.Read(got)
		_ = f.Close()
		if string(got[:n]) != string(want) {
			t.Errorf("%s differs between embed and disk", p)
		}
	}
	if err := migrate.Validate(dir); err != nil {
		t.Fatalf("atlas.sum does not validate against the embedded files: %v", err)
	}
}
