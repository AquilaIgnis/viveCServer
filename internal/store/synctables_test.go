package store

import (
	"regexp"
	"strings"
	"testing"
)

// TestEveryKindRendersEveryFieldItStores is the guard on this package's one real risk.
//
// A kind's fields are written by hand-written SQL in one place and read back by a hand-written JSON
// expression in another. If the two drift, a push succeeds, a pull returns the entity without the
// field, and nothing fails until a user notices a colour has reset.
func TestEveryKindRendersEveryFieldItStores(t *testing.T) {
	for _, kind := range SyncKindsInApplyOrder() {
		for jsonKey := range kind.fieldKeys {
			if !strings.Contains(kind.changeJSON, "'"+jsonKey+"'") {
				t.Errorf("kind %q stores %q but never renders it on pull", kind.Name, jsonKey)
			}
		}
		for reservedKey := range reservedChangeKeys {
			// baseVersion is the only envelope key a client sends and never receives: the response
			// carries the version the server assigned instead.
			if reservedKey == "baseVersion" {
				continue
			}
			if !strings.Contains(kind.changeJSON, "'"+reservedKey+"'") {
				t.Errorf("kind %q does not render the envelope key %q", kind.Name, reservedKey)
			}
		}
	}
}

// TestRenderedKeysAreAllRecognised is the same check in reverse, so a typo in a rendering
// expression cannot invent a field name no client is looking for.
func TestRenderedKeysAreAllRecognised(t *testing.T) {
	// Key position only — a quoted token followed by a comma, which is what every argument pair in
	// `jsonb_build_object` looks like. Matching every quoted token instead would read the argument
	// of a SQL function as a key the moment a kind renders through one, as `page_content` does with
	// `encode(doc, 'base64')`.
	quotedToken := regexp.MustCompile(`'([A-Za-z][A-Za-z0-9_]*)'\s*,`)

	for _, kind := range SyncKindsInApplyOrder() {
		for _, match := range quotedToken.FindAllStringSubmatch(kind.changeJSON, -1) {
			token := match[1]
			if token == kind.Name || IsReservedChangeKey(token) || kind.ClaimsField(token) {
				continue
			}
			t.Errorf("kind %q renders %q, which is neither one of its fields nor an envelope key", kind.Name, token)
		}
	}
}

// TestParentsRankBeforeChildren: the apply order is what lets a batch carry a notebook and the
// section inside it, and the pull order is what lets a client apply a delta without sorting it.
func TestParentsRankBeforeChildren(t *testing.T) {
	for _, kind := range SyncKindsInApplyOrder() {
		if kind.ParentKind == "" {
			continue
		}
		parent, known := SyncKindByName(kind.ParentKind)
		if !known {
			t.Fatalf("kind %q names an unregistered parent %q", kind.Name, kind.ParentKind)
		}
		if parent.Rank >= kind.Rank {
			t.Errorf("kind %q ranks %d, at or before its parent %q at %d", kind.Name, kind.Rank, parent.Name, parent.Rank)
		}
	}
}

// TestFieldKeysComeFromTheStructTags proves the reflection that decides what counts as an
// unrecognised field is actually reading the tags.
func TestFieldKeysComeFromTheStructTags(t *testing.T) {
	notebook, _ := SyncKindByName("notebook")
	for _, expected := range []string{"name", "colorArgb", "sortIndex", "expanded", "createdAt"} {
		if !notebook.ClaimsField(expected) {
			t.Errorf("notebook does not claim %q, so a client sending it would have it stored as an unknown field", expected)
		}
	}
	if notebook.ClaimsField("Name") || notebook.ClaimsField("icon") {
		t.Error("notebook claims a field it has no column for")
	}
}

// TestTheDeltaQueryReadsEveryKind: a kind missing from the union would be stored and then never
// delivered to anybody.
func TestTheDeltaQueryReadsEveryKind(t *testing.T) {
	for _, kind := range SyncKindsInApplyOrder() {
		if !strings.Contains(selectDeltaStatement, "FROM "+kind.Table+"\n") {
			t.Errorf("the delta query never reads %s", kind.Table)
		}
	}
	if !strings.Contains(selectDeltaStatement, "ORDER BY change_seq, kind_rank, id") {
		t.Error("the delta query does not order parents before children within a sequence value")
	}
}

func TestValidateEntityIDRefusesWhatPostgresCannotStore(t *testing.T) {
	if err := ValidateEntityID(""); err == nil {
		t.Error("an empty id was accepted")
	}
	if err := ValidateEntityID("page\x00id"); err == nil {
		t.Error("an id containing a NUL byte was accepted, which would fail the whole transaction")
	}
	if err := ValidateEntityID(strings.Repeat("a", maxIDChars+1)); err == nil {
		t.Error("an over-long id was accepted")
	}
	if err := ValidateEntityID("00000000-0000-4000-8000-000000000001"); err != nil {
		t.Errorf("an ordinary id was refused: %v", err)
	}
}

// TestAccountLockKeysAreDistinctPerAccount is not a correctness requirement — a collision only
// costs concurrency — but a hash that returned the same key for everyone would silently serialise
// every account on one server against every other.
func TestAccountLockKeysAreDistinctPerAccount(t *testing.T) {
	first := accountLockKey("a62c615f-5a73-47bb-b704-ad49cf527ec2")
	second := accountLockKey("7af9be36-8f89-4b31-bc78-3ef246837469")
	if first == second {
		t.Fatal("two different accounts hash to the same advisory lock key")
	}
	if accountLockKey("a62c615f-5a73-47bb-b704-ad49cf527ec2") != first {
		t.Fatal("the lock key for one account is not stable")
	}
}
