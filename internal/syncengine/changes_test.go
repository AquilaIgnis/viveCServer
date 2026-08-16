package syncengine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AquilaIgnis/viveCServer/internal/store"
)

func rawChange(t *testing.T, fields map[string]any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encoding a test change: %v", err)
	}
	return encoded
}

func TestUnrecognisedFieldsAreCollectedAndEnvelopeKeysAreNot(t *testing.T) {
	decoded := decodeChanges([]json.RawMessage{rawChange(t, map[string]any{
		"kind":        "notebook",
		"id":          "notebook-1",
		"baseVersion": 0,
		"updatedAt":   17,
		"name":        "Work",
		"colorArgb":   -1,
		"sortIndex":   0,
		"expanded":    true,
		"createdAt":   17,

		// What a newer client ships, and what it must never be able to overwrite.
		"icon":    "star",
		"version": 999,
		"seq":     999,
	})})

	if decoded[0].rejection != nil {
		t.Fatalf("a valid change was refused: %+v", decoded[0].rejection)
	}

	var extra map[string]any
	if err := json.Unmarshal([]byte(decoded[0].envelope.Extra), &extra); err != nil {
		t.Fatalf("stored extra is not an object: %q", decoded[0].envelope.Extra)
	}
	if extra["icon"] != "star" {
		t.Errorf("an unrecognised field was dropped: %v", extra)
	}
	if _, present := extra["version"]; present {
		t.Error("a client-supplied version was stored as an unrecognised field")
	}
	if _, present := extra["seq"]; present {
		t.Error("a client-supplied seq was stored as an unrecognised field")
	}
	if _, present := extra["name"]; present {
		t.Error("a field the kind has a column for was also stored as an unrecognised one")
	}
}

func TestAChangeWithNothingUnrecognisedStoresAnEmptyObject(t *testing.T) {
	decoded := decodeChanges([]json.RawMessage{rawChange(t, map[string]any{
		"kind": "notebook", "id": "notebook-1", "baseVersion": 0, "updatedAt": 1,
		"name": "Work", "colorArgb": 0, "sortIndex": 0, "expanded": false, "createdAt": 1,
	})})

	// Never null: the column is NOT NULL and `extra || …` on pull would erase the whole object.
	if decoded[0].envelope.Extra != "{}" {
		t.Fatalf("extra = %q, want an empty object", decoded[0].envelope.Extra)
	}
}

func TestOversizedUnrecognisedFieldsAreRefused(t *testing.T) {
	decoded := decodeChanges([]json.RawMessage{rawChange(t, map[string]any{
		"kind": "notebook", "id": "notebook-1", "baseVersion": 0, "updatedAt": 1,
		"name": "Work", "colorArgb": 0, "sortIndex": 0, "expanded": false, "createdAt": 1,
		"blob": strings.Repeat("x", maxExtraBytes+1),
	})})

	if decoded[0].rejection == nil || decoded[0].rejection.Reason != ReasonTooLarge {
		t.Fatalf("rejection = %+v, want %q", decoded[0].rejection, ReasonTooLarge)
	}
}

func TestChangesSortParentsBeforeChildren(t *testing.T) {
	decoded := decodeChanges([]json.RawMessage{
		rawChange(t, map[string]any{"kind": "page", "id": "page-1", "sectionId": "section-1", "title": "T", "sortIndex": 0, "preview": "", "createdAt": 1, "updatedAt": 1}),
		rawChange(t, map[string]any{"kind": "unknown_kind", "id": "x"}),
		rawChange(t, map[string]any{"kind": "section", "id": "section-1", "notebookId": "notebook-1", "name": "S", "colorArgb": 0, "sortIndex": 0, "createdAt": 1, "updatedAt": 1}),
		rawChange(t, map[string]any{"kind": "notebook", "id": "notebook-1", "name": "N", "colorArgb": 0, "sortIndex": 0, "expanded": true, "createdAt": 1, "updatedAt": 1}),
	})

	order := make([]string, 0, len(decoded))
	for _, change := range decoded {
		if change.kind == nil {
			order = append(order, "unresolved")
			continue
		}
		order = append(order, change.kind.Name)
	}
	if strings.Join(order, " ") != "notebook section page unresolved" {
		t.Fatalf("apply order = %v, want parents first and unresolvable kinds last", order)
	}
}

func TestOrderWithinOneKindIsTheOrderTheClientSent(t *testing.T) {
	notebook := func(id string) json.RawMessage {
		return rawChange(t, map[string]any{
			"kind": "notebook", "id": id, "name": "N", "colorArgb": 0,
			"sortIndex": 0, "expanded": true, "createdAt": 1, "updatedAt": 1,
		})
	}
	decoded := decodeChanges([]json.RawMessage{notebook("c"), notebook("a"), notebook("b")})

	order := make([]string, 0, len(decoded))
	for _, change := range decoded {
		order = append(order, change.envelope.ID)
	}
	if strings.Join(order, " ") != "c a b" {
		t.Fatalf("order within a kind = %v, want the client's own order", order)
	}
}

func TestRefusalsCarryTheReasonAClientCanActOn(t *testing.T) {
	valid := map[string]any{
		"kind": "notebook", "id": "notebook-1", "baseVersion": 0, "updatedAt": 1,
		"name": "Work", "colorArgb": 0, "sortIndex": 0, "expanded": true, "createdAt": 1,
	}
	withField := func(key string, value any) map[string]any {
		copied := make(map[string]any, len(valid)+1)
		for existingKey, existingValue := range valid {
			copied[existingKey] = existingValue
		}
		copied[key] = value
		return copied
	}

	cases := []struct {
		name       string
		change     map[string]any
		wantReason string
	}{
		{"unknown kind", withField("kind", "page_content"), ReasonMalformed},
		{"empty id", withField("id", ""), ReasonMalformed},
		{"negative base version", withField("baseVersion", -1), ReasonMalformed},
		{"wrong field type", withField("colorArgb", "blue"), ReasonMalformed},
		{"colour outside int32", withField("colorArgb", int64(1)<<40), ReasonMalformed},
		{"NUL in a name", withField("name", "before\x00after"), ReasonMalformed},
		{"over-long name", withField("name", strings.Repeat("a", 513)), ReasonTooLarge},
		{"missing parent id", map[string]any{
			"kind": "section", "id": "section-1", "baseVersion": 0, "updatedAt": 1,
			"name": "S", "colorArgb": 0, "sortIndex": 0, "createdAt": 1,
		}, ReasonMalformed},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			decoded := decodeChanges([]json.RawMessage{rawChange(t, testCase.change)})
			if decoded[0].rejection == nil {
				t.Fatalf("%s was accepted", testCase.name)
			}
			if decoded[0].rejection.Reason != testCase.wantReason {
				t.Fatalf("%s rejected as %q, want %q (%s)",
					testCase.name, decoded[0].rejection.Reason, testCase.wantReason, decoded[0].rejection.Message)
			}
		})
	}
}

func TestTheSameEntityTwiceIsRefusedOnceAndAcceptedOnce(t *testing.T) {
	notebook := func(name string) json.RawMessage {
		return rawChange(t, map[string]any{
			"kind": "notebook", "id": "notebook-1", "baseVersion": 0, "updatedAt": 1,
			"name": name, "colorArgb": 0, "sortIndex": 0, "expanded": true, "createdAt": 1,
		})
	}
	decoded := decodeChanges([]json.RawMessage{notebook("first"), notebook("second")})

	if decoded[0].rejection != nil {
		t.Fatalf("the first copy was refused: %+v", decoded[0].rejection)
	}
	if decoded[1].rejection == nil || decoded[1].rejection.Reason != ReasonMalformed {
		t.Fatalf("the second copy = %+v, want a malformed rejection", decoded[1].rejection)
	}
}

// TestEveryKindDecodesItsOwnParent proves the parent used for the missing_parent check is the one
// the kind's field says, rather than whichever field happens to be first.
func TestEveryKindDecodesItsOwnParent(t *testing.T) {
	section, _ := store.SyncKindByName("section")
	decodedSection := decodeChanges([]json.RawMessage{rawChange(t, map[string]any{
		"kind": section.Name, "id": "section-1", "baseVersion": 0, "updatedAt": 1,
		"notebookId": "notebook-7", "name": "S", "colorArgb": 0, "sortIndex": 0, "createdAt": 1,
	})})
	if parent := decodedSection[0].fields.ParentID(); parent != "notebook-7" {
		t.Errorf("section parent = %q, want notebook-7", parent)
	}

	decodedPage := decodeChanges([]json.RawMessage{rawChange(t, map[string]any{
		"kind": "page", "id": "page-1", "baseVersion": 0, "updatedAt": 1,
		"sectionId": "section-9", "title": "T", "sortIndex": 0, "preview": "", "createdAt": 1,
	})})
	if parent := decodedPage[0].fields.ParentID(); parent != "section-9" {
		t.Errorf("page parent = %q, want section-9", parent)
	}

	decodedNotebook := decodeChanges([]json.RawMessage{rawChange(t, map[string]any{
		"kind": "notebook", "id": "notebook-1", "baseVersion": 0, "updatedAt": 1,
		"name": "N", "colorArgb": 0, "sortIndex": 0, "expanded": true, "createdAt": 1,
	})})
	if parent := decodedNotebook[0].fields.ParentID(); parent != "" {
		t.Errorf("notebook parent = %q, want none", parent)
	}
}
