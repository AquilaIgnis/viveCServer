package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/AquilaIgnis/viveCServer/internal/store"
)

// TestThePoolKeepsAWarmConnectionUnlessTheURLSaysOtherwise: the server keeps a connection open
// through quiet periods by default, and an operator who set pool_min_conns in VIVE_DATABASE_URL —
// zero included — gets exactly that.
func TestThePoolKeepsAWarmConnectionUnlessTheURLSaysOtherwise(t *testing.T) {
	databaseURL := testDatabaseURL(t)
	ctx := context.Background()

	defaulted, err := store.OpenPool(ctx, databaseURL)
	if err != nil {
		t.Fatalf("opening a pool: %v", err)
	}
	defer defaulted.Close()
	if minimum := defaulted.Config().MinConns; minimum < 1 {
		t.Fatalf("a pool opened without pool_min_conns keeps %d connections, want at least 1", minimum)
	}

	separator := "?"
	if strings.Contains(databaseURL, "?") {
		separator = "&"
	}
	explicit, err := store.OpenPool(ctx, databaseURL+separator+"pool_min_conns=0")
	if err != nil {
		t.Fatalf("opening a pool with an explicit minimum: %v", err)
	}
	defer explicit.Close()
	if minimum := explicit.Config().MinConns; minimum != 0 {
		t.Fatalf("pool_min_conns=0 in the URL left a minimum of %d, want the operator's 0", minimum)
	}
}
