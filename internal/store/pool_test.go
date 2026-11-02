package store

import "testing"

// TestThePoolMinimumIsAShareOfTheMaximumWithinBounds: at least one, so a small pool still keeps a
// connection through a quiet period, and at most four, so a large one does not hold idle backends
// against PostgreSQL's connection limit.
func TestThePoolMinimumIsAShareOfTheMaximumWithinBounds(t *testing.T) {
	for _, testCase := range []struct {
		maximumConnections int32
		want               int32
	}{
		{1, 1},
		{4, 1},
		{8, 1},
		{16, 2},
		{32, 4},
		{100, 4},
	} {
		if got := minimumPoolConnections(testCase.maximumConnections); got != testCase.want {
			t.Errorf("minimumPoolConnections(%d) = %d, want %d", testCase.maximumConnections, got, testCase.want)
		}
	}
}
