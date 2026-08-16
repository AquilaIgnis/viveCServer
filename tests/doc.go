// Package tests holds the integration suite: the behaviour that only means anything against a real
// PostgreSQL and a real HTTP listener.
//
// The unit tests beside each package cover what can be decided without a database. What cannot is
// everything this server was built to get right — that a sequence value is allocated exactly once
// under concurrency, that a reader never steps over a change, that two devices converge. Those are
// properties of PostgreSQL's locking as much as of this code, so a fake would only prove that the
// fake agrees with itself. syncPlan.md §9 makes this harness the deliverable of S2 rather than an
// afterthought for the same reason.
//
// Set VIVE_TEST_DATABASE_URL to run it. There is no fallback in-memory mode on purpose.
package tests
