package db_test

import (
	"context"
	"testing"

	"github.com/sashasagebd/pokewordle-api/internal/db"
	"github.com/sashasagebd/pokewordle-api/internal/dbtest"
)

func TestMigrateTwiceIsANoOp(t *testing.T) {
	pool := dbtest.Pool(t) // migrates once
	applied, err := db.Migrate(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Fatalf("second run applied %v, want nothing", applied)
	}
}
