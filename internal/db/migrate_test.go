package db

import (
	"testing"
	"testing/fstest"
)

func TestLoadMigrationsEmbedded(t *testing.T) {
	ms, err := loadMigrations(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 {
		t.Fatal("no migrations embedded")
	}
}

func TestLoadMigrationsRejectsGapsAndBadNames(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"gap": {
			"m/0001_a.sql": {Data: []byte("SELECT 1")},
			"m/0003_c.sql": {Data: []byte("SELECT 1")},
		},
		"bad name": {
			"m/1-a.sql": {Data: []byte("SELECT 1")},
		},
		"duplicate": {
			"m/0001_a.sql": {Data: []byte("SELECT 1")},
			"m/0001_b.sql": {Data: []byte("SELECT 1")},
		},
	}
	for name, fsys := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadMigrations(fsys, "m"); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}
