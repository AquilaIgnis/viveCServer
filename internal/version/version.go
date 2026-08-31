// Package version holds the one place this server says which build it is.
//
// It is its own package rather than a constant inside httpapi because the answer belongs to the
// server, not to the admin panel: the CLI and any future health or diagnostic surface must be able
// to state the same numbers without importing the web interface to get at it.
package version

const (
	// Current is the server's own version, shown in the admin panel.
	//
	// It is a hand-written constant, not something derived from Git at build time. The panel exists
	// so that a person who runs this server on their own machine can tell what they are running, and
	// a commit hash or a dirty-tree suffix does not answer that question -- "1.0.1" is what they
	// compare against a release note. Bump it in the same change that ships behaviour an operator
	// could notice.
	Current = "1.0.1"

	// OpenApi is the version of the device/sync contract in docs/openapi.yaml, and it moves on its
	// own schedule.
	//
	// These are two different questions and deliberately two different numbers: Current says which
	// build is running, OpenApi says which contract a tablet can expect it to answer. Most releases
	// move only the first -- an admin panel change is invisible to every device. When a sync
	// endpoint does change, this and the `info.version` field of docs/openapi.yaml must move in the
	// same commit, or the panel reports a contract the server does not implement.
	OpenApi = "0.7.0"
)
