package store

// This file holds the machinery every synced table shares: the envelope on each row, the registry
// of entity kinds, and the queries whose text is identical whatever the kind. The kind-specific SQL
// lives beside the fields it writes, in notebooks.go, sections.go and pages.go.
//
// The field structs in those files carry JSON tags, which puts a wire contract in the store layer
// on purpose. A synced entity's stored shape and its shape on the wire are the same thing: the pull
// path renders rows straight to JSON inside SQL (see each kind's changeJSON), so a second set of
// field names in httpapi would be one more place for the two to drift apart without a test noticing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Limits mirror NotebookTransferManager's, so a notebook that imports from a `.vive` bundle also
// syncs and neither path can produce a corpus the other refuses (syncPlan.md §6).
const (
	maxIDChars      = 512  // NotebookTransferManager.MAX_ID_CHARS
	maxNameChars    = 512  // NotebookTransferManager.MAX_NAME_CHARS
	maxPreviewChars = 4096 // NotebookTransferManager.MAX_PREVIEW_CHARS
)

// ErrTooLarge marks a validation failure that is about size rather than shape, so a caller can
// answer `too_large` instead of `malformed` without matching on message text.
var ErrTooLarge = errors.New("value is too large")

// Querier is the part of pgx these queries need, so each one runs unchanged against the pool or
// inside the push transaction. pgx exports no such interface of its own.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ChangeEnvelope is the half of a synced change that every kind has in common.
type ChangeEnvelope struct {
	AccountID string
	ID        string
	Version   int64
	ChangeSeq int64
	DeletedAt *int64
	UpdatedAt int64

	// The device that pushed this write, for diagnostics only.
	LastWriter string

	// Unrecognised fields, as a JSON object. Never null: an absent extra is `{}`.
	Extra string
}

// EntityFields is the kind-specific half of a synced change.
//
// One implementation per synced kind, each owning its own INSERT text. Generating that SQL from a
// column list would be shorter, but it would put the column order in one file and the argument
// order in another, and getting them out of step writes a title into a preview column with nothing
// failing.
type EntityFields interface {
	// ParentID is the entity this one hangs from, or "" for a kind with no parent.
	ParentID() string

	// Validate reports why these fields cannot be stored, or nil. A size failure wraps ErrTooLarge.
	Validate() error

	// upsertStatement writes the entity under optimistic concurrency: it applies only while the
	// stored version still equals baseVersion, and reports no affected rows otherwise.
	upsertStatement(envelope ChangeEnvelope, baseVersion int64) (statement string, arguments []any)
}

// BlobReferencingFields is implemented by a kind whose row is only valid while the server holds
// certain attachment blobs (SD7).
//
// An optional interface rather than a method on every kind: three of the eight kinds could not
// answer it meaningfully, and a `return nil, nil` in each of them is five more places to look when
// asking which kinds actually reference bytes.
type BlobReferencingFields interface {
	// RequiredBlobDigests returns the digests that must already be stored, in wire form. An error
	// means the entity names something that cannot be a digest at all.
	RequiredBlobDigests(entityID string) ([]string, error)
}

// BlobConsistencyChecker is implemented by a kind that has something more to say about the blobs it
// references once they are known to be present.
type BlobConsistencyChecker interface {
	CheckBlobPresence(entityID string, present map[string]BlobPresence) error
}

// DeletionRetainer is implemented by a kind for which a client's tombstone can mean "stop holding
// this on my device" rather than "erase this from the account".
//
// Optional, and today only `notebook` implements it. Every other kind's delete is a delete: an
// inked stroke that a client rubbed out is gone, and a server that second-guessed that would be
// storing a document its own protocol says does not exist.
type DeletionRetainer interface {
	// RetainOnDelete reports whether the tombstone should be stored as a retention instead, and
	// mutates the fields into the state to store when it is. `stored` is the row as a pull would
	// render it, or empty when the account holds none.
	RetainOnDelete(stored json.RawMessage, deletedAt int64) bool
}

// auxiliaryStatement is a write a kind makes outside its own table.
type auxiliaryStatement struct {
	sql       string
	arguments []any
}

// auxiliaryWriter is implemented by a kind that maintains a second table alongside its own row —
// today only `pageContent`, which keeps `page_blob_refs` in step with the document that names them.
//
// The statements are queued into the same transaction and the same pipeline as the row itself,
// which is what makes "a document and its reference set are never separately visible" a property of
// the design rather than of remembering to write both.
type auxiliaryWriter interface {
	auxiliaryStatements(envelope ChangeEnvelope) []auxiliaryStatement
}

// SyncKind describes one synced entity kind to the generic parts of the engine.
type SyncKind struct {
	// Name is the `kind` discriminator on the wire.
	Name string

	// Rank orders kinds so a parent is always applied and emitted before its children. It is not
	// alphabetical for a reason: sorting by name would put pages before sections.
	Rank int

	Table string

	// ParentKind is the kind whose ids ParentID returns, or "" when the kind has no parent.
	ParentKind string

	// changeJSON is the SQL expression that renders a row of this table into the object a client
	// receives. `extra ||` comes first so that a stored unknown field can never overwrite the
	// identity or version the server assigned.
	changeJSON string

	newFields func() EntityFields

	// fieldKeys is every JSON key the kind's own struct claims, derived once from its tags so that
	// adding a field cannot forget to stop it being treated as unknown.
	fieldKeys map[string]struct{}
}

// NewFields returns an empty value to decode this kind's fields into.
func (kind *SyncKind) NewFields() EntityFields {
	return kind.newFields()
}

// ClaimsField reports whether the kind stores a JSON key in a column of its own, which is what
// makes every other key an unrecognised one bound for `extra`.
func (kind *SyncKind) ClaimsField(jsonKey string) bool {
	_, claimed := kind.fieldKeys[jsonKey]
	return claimed
}

// reservedChangeKeys are the envelope keys, which belong to the protocol rather than to any kind
// and must never be stored as unrecognised fields.
//
// `version` and `seq` are in the list although a push never sets them: a client that echoes a
// pulled change straight back would otherwise store a stale version inside `extra`, where it would
// be returned to every other device for ever.
var reservedChangeKeys = map[string]struct{}{
	"kind":        {},
	"id":          {},
	"baseVersion": {},
	"version":     {},
	"seq":         {},
	"deletedAt":   {},
	"updatedAt":   {},
}

// IsReservedChangeKey reports whether a JSON key belongs to the change envelope.
func IsReservedChangeKey(jsonKey string) bool {
	_, reserved := reservedChangeKeys[jsonKey]
	return reserved
}

// syncKinds is the registry, in apply order. Each kind is one entry here and one file beside this
// one — which is what page_content (S3), the three ink kinds (S4) and attachment (S5) each turned
// out to cost.
var syncKinds = []*SyncKind{
	{
		Name:       "notebook",
		Rank:       0,
		Table:      "notebooks",
		ParentKind: "",
		changeJSON: notebookChangeJSON,
		newFields:  func() EntityFields { return &NotebookFields{} },
	},
	{
		Name:       "section",
		Rank:       1,
		Table:      "sections",
		ParentKind: "notebook",
		changeJSON: sectionChangeJSON,
		newFields:  func() EntityFields { return &SectionFields{} },
	},
	{
		Name:       "page",
		Rank:       2,
		Table:      "pages",
		ParentKind: "section",
		changeJSON: pageChangeJSON,
		newFields:  func() EntityFields { return &PageFields{} },
	},
	{
		Name:       "pageContent",
		Rank:       3,
		Table:      "page_content",
		ParentKind: "page",
		changeJSON: pageContentChangeJSON,
		newFields:  func() EntityFields { return &PageContentFields{} },
	},
	// The three ink kinds hang from a page and never from each other. An erase names the strokes it
	// affected, but as inert ids rather than as a parent: enforcing them would wedge a client the
	// first time a target arrived in a later delta page or had already been purged (inkoperations.go,
	// validateInkTargets).
	{
		Name:       "inkStroke",
		Rank:       4,
		Table:      "ink_strokes",
		ParentKind: "page",
		changeJSON: inkStrokeChangeJSON,
		newFields:  func() EntityFields { return &InkStrokeFields{} },
	},
	{
		Name:       "inkErase",
		Rank:       5,
		Table:      "ink_erases",
		ParentKind: "page",
		changeJSON: inkEraseChangeJSON,
		newFields:  func() EntityFields { return &InkEraseFields{} },
	},
	{
		Name:       "inkMove",
		Rank:       6,
		Table:      "ink_moves",
		ParentKind: "page",
		changeJSON: inkMoveChangeJSON,
		newFields:  func() EntityFields { return &InkMoveFields{} },
	},
	// Attachments hang from nothing. One picture can appear on several pages, so there is no parent
	// whose existence could be checked — what is checked instead is that the server holds the bytes
	// the id names (SD7). Last in the apply order because a document that references a picture is
	// only accepted once that picture's bytes are stored, and the bytes arrive over `/v1/blobs`
	// before either row does.
	{
		Name:       "attachment",
		Rank:       7,
		Table:      "attachments",
		ParentKind: "",
		changeJSON: attachmentChangeJSON,
		newFields:  func() EntityFields { return &AttachmentFields{} },
	},
}

var syncKindsByName = func() map[string]*SyncKind {
	byName := make(map[string]*SyncKind, len(syncKinds))
	for _, kind := range syncKinds {
		kind.fieldKeys = jsonFieldKeysOf(kind.newFields())
		byName[kind.Name] = kind
	}
	return byName
}()

// SyncKindByName resolves the `kind` discriminator a client sent.
func SyncKindByName(name string) (*SyncKind, bool) {
	kind, known := syncKindsByName[name]
	return kind, known
}

// SyncKindsInApplyOrder returns every kind, parents before children.
func SyncKindsInApplyOrder() []*SyncKind {
	return syncKinds
}

// jsonFieldKeysOf reads a field struct's JSON tags. Deriving the set rather than writing it out
// twice means a new column cannot be added and silently keep arriving as an unrecognised field.
func jsonFieldKeysOf(fields EntityFields) map[string]struct{} {
	keys := make(map[string]struct{})
	for _, field := range reflect.VisibleFields(reflect.TypeOf(fields).Elem()) {
		jsonName, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if jsonName != "" && jsonName != "-" {
			keys[jsonName] = struct{}{}
		}
	}
	return keys
}

// validateSyncText is the check every stored string goes through.
func validateSyncText(fieldName string, value string, maxChars int) error {
	if strings.ContainsRune(value, 0) {
		// PostgreSQL `text` cannot hold a NUL byte and JSON can spell one as \u0000. Letting it
		// reach the driver would fail the whole transaction rather than the one entity, turning a
		// single bad row into a batch that can never succeed and that the client retries for ever.
		return fmt.Errorf("%s must not contain a NUL character", fieldName)
	}
	if utf8.RuneCountInString(value) > maxChars {
		return fmt.Errorf("%w: %s is longer than %d characters", ErrTooLarge, fieldName, maxChars)
	}
	return nil
}

// ValidateEntityID checks the id a change is addressed to, which no kind owns because every kind
// has one.
func ValidateEntityID(id string) error {
	if id == "" {
		return errors.New("id must not be empty")
	}
	return validateSyncText("id", id, maxIDChars)
}

// accountLockNamespace separates this lock from every other advisory lock the server might take.
//
// PostgreSQL keeps the one-bigint and two-int4 advisory key spaces apart — "note that these two
// key spaces do not overlap" — so a push lock can never collide with the single-key setup lock in
// accounts.go however the account hashes.
const accountLockNamespace int32 = 0x56495645 // "VIVE"

// BeginAccountWrite opens the transaction every push runs in and takes the account's advisory lock.
//
// The lock is what makes sequence order equal commit order (SD2). It is held until the transaction
// ends, so a writer that allocated a sequence value always commits before the next writer can
// allocate the following one, and a reader can therefore never consume a cursor value while an
// earlier one is still invisible. One account is one person with a handful of devices, so
// serialising its writers costs nothing measurable and removes the whole class of bug.
func BeginAccountWrite(ctx context.Context, pool *pgxpool.Pool, accountID string) (pgx.Tx, error) {
	transaction, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("starting a push transaction: %w", err)
	}

	if _, err := transaction.Exec(ctx,
		`SELECT pg_advisory_xact_lock($1, $2)`,
		accountLockNamespace, accountLockKey(accountID),
	); err != nil {
		_ = transaction.Rollback(ctx)
		return nil, fmt.Errorf("locking an account for writing: %w", err)
	}

	return transaction, nil
}

// accountLockKey hashes an account id into the 32 bits an advisory lock key holds.
//
// Computed here rather than with PostgreSQL's hashtext(), which is an undocumented internal whose
// output is not promised to be stable across major versions. Two accounts that collide serialise
// against each other and lose a little concurrency; nothing about correctness depends on the hash
// being injective.
func accountLockKey(accountID string) int32 {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(accountID))
	return int32(hash.Sum32())
}

// AllocateChangeSeq bumps the account's counter and returns the value this push will stamp on every
// row it writes. It must be called inside BeginAccountWrite's transaction.
func AllocateChangeSeq(ctx context.Context, transaction pgx.Tx, accountID string) (int64, error) {
	const statement = `
		UPDATE accounts
		SET change_seq = change_seq + 1
		WHERE id = $1::uuid
		RETURNING change_seq`

	var allocated int64
	if err := transaction.QueryRow(ctx, statement, accountID).Scan(&allocated); err != nil {
		return 0, fmt.Errorf("allocating a change sequence: %w", err)
	}
	return allocated, nil
}

// ReadChangeSeq returns an account's current cursor. It is one primary-key read for launch and
// event-stream reconnect catch-up; an idle connected client waits on the change feed instead.
func ReadChangeSeq(ctx context.Context, database Querier, accountID string) (int64, error) {
	var cursor int64
	err := database.QueryRow(ctx, `SELECT change_seq FROM accounts WHERE id = $1::uuid`, accountID).Scan(&cursor)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrAccountNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("reading a change cursor: %w", err)
	}
	return cursor, nil
}

// CurrentChange is a stored row as the protocol describes it.
type CurrentChange struct {
	Version int64

	// Change is the row rendered exactly as a pull would return it, so a rejection can hand the
	// client the current state without a second query or a second rendering path.
	Change json.RawMessage
}

// SelectCurrentChanges reads the stored rows behind a batch's ids, keyed by id.
func SelectCurrentChanges(
	ctx context.Context,
	database Querier,
	kind *SyncKind,
	accountID string,
	entityIDs []string,
) (map[string]CurrentChange, error) {
	current := make(map[string]CurrentChange, len(entityIDs))
	if len(entityIDs) == 0 {
		return current, nil
	}

	statement := `SELECT id, version, ` + kind.changeJSON +
		` FROM ` + kind.Table + ` WHERE account_id = $1::uuid AND id = ANY($2::text[])`

	rows, err := database.Query(ctx, statement, accountID, entityIDs)
	if err != nil {
		return nil, fmt.Errorf("reading current %s rows: %w", kind.Name, err)
	}
	defer rows.Close()

	for rows.Next() {
		var entityID string
		var stored CurrentChange
		if err := rows.Scan(&entityID, &stored.Version, &stored.Change); err != nil {
			return nil, fmt.Errorf("reading current %s rows: %w", kind.Name, err)
		}
		current[entityID] = stored
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading current %s rows: %w", kind.Name, err)
	}
	return current, nil
}

// SelectExistingIDs reports which of the given ids the account already has rows for.
//
// Used for the parent check. Existence is the question, not liveness: a tombstoned parent is still
// a row, the foreign key is still satisfied, and what a deleted subtree means is the client's
// decision rather than this server's.
func SelectExistingIDs(
	ctx context.Context,
	database Querier,
	kind *SyncKind,
	accountID string,
	entityIDs []string,
) (map[string]struct{}, error) {
	existing := make(map[string]struct{}, len(entityIDs))
	if len(entityIDs) == 0 {
		return existing, nil
	}

	statement := `SELECT id FROM ` + kind.Table + ` WHERE account_id = $1::uuid AND id = ANY($2::text[])`

	rows, err := database.Query(ctx, statement, accountID, entityIDs)
	if err != nil {
		return nil, fmt.Errorf("checking %s parents: %w", kind.Name, err)
	}
	defer rows.Close()

	for rows.Next() {
		var entityID string
		if err := rows.Scan(&entityID); err != nil {
			return nil, fmt.Errorf("checking %s parents: %w", kind.Name, err)
		}
		existing[entityID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("checking %s parents: %w", kind.Name, err)
	}
	return existing, nil
}

// PendingWrite is one entity that survived validation and the version check.
type PendingWrite struct {
	Kind     string
	Envelope ChangeEnvelope
	Fields   EntityFields

	// BaseVersion is what the client believed was stored. It goes into the statement's WHERE as
	// well, so the write is guarded even though the caller already checked it.
	BaseVersion int64
}

// ErrConcurrentWrite means a guarded upsert affected no row: the stored version moved after the
// caller read it. Inside BeginAccountWrite's lock that cannot happen, so it signals a bug in the
// caller rather than a losing race, and failing the push is better than half-applying it.
var ErrConcurrentWrite = errors.New("a guarded write affected no row")

// WriteChanges applies every pending write as one pipelined round trip.
//
// Pipelined rather than sent one statement at a time because a batch is capped at 512 entities, and
// 512 round trips to a database in another container is the difference between a push that feels
// instant and one that does not. They all share the caller's transaction, so the batch is still
// atomic.
func WriteChanges(ctx context.Context, transaction pgx.Tx, writes []PendingWrite) error {
	if len(writes) == 0 {
		return nil
	}

	// queued mirrors the pipeline one for one, because every result has to be read back and only
	// the guarded upserts are required to have affected a row: a kind's auxiliary statements may
	// legitimately affect none, as clearing the references of a page that had none does.
	type queuedResult struct {
		kind     string
		entityID string
		guarded  bool
	}

	pipeline := &pgx.Batch{}
	queued := make([]queuedResult, 0, len(writes))
	for _, write := range writes {
		statement, arguments := write.Fields.upsertStatement(write.Envelope, write.BaseVersion)
		pipeline.Queue(statement, arguments...)
		queued = append(queued, queuedResult{kind: write.Kind, entityID: write.Envelope.ID, guarded: true})

		auxiliary, writesElsewhere := write.Fields.(auxiliaryWriter)
		if !writesElsewhere {
			continue
		}
		for _, statement := range auxiliary.auxiliaryStatements(write.Envelope) {
			pipeline.Queue(statement.sql, statement.arguments...)
			queued = append(queued, queuedResult{kind: write.Kind, entityID: write.Envelope.ID})
		}
	}

	results := transaction.SendBatch(ctx, pipeline)
	var firstError error
	for _, write := range queued {
		commandTag, err := results.Exec()
		switch {
		case firstError != nil:
			// Already failed; the remaining results still have to be drained.
		case err != nil:
			firstError = fmt.Errorf("writing %s %q: %w", write.kind, write.entityID, err)
		case write.guarded && commandTag.RowsAffected() != 1:
			firstError = fmt.Errorf("%w: %s %q", ErrConcurrentWrite, write.kind, write.entityID)
		}
	}

	// Every result has to be read before the connection is usable again, so Close runs whatever is
	// left even when the loop above already found a failure.
	if err := results.Close(); err != nil && firstError == nil {
		firstError = fmt.Errorf("finishing a write pipeline: %w", err)
	}
	return firstError
}

// DeltaRow is one change on its way to a pulling client.
type DeltaRow struct {
	ChangeSeq int64
	Change    json.RawMessage
}

// MaxDeltaBytes is the budget one pull response spends on rows before it stops at the next sequence
// boundary.
//
// Comfortably above one push (4 MB, §7) plus what base64 adds, so it never cuts inside the work of
// a single client and only ever draws the line between two of them. Lower would make a large push
// unpullable; much higher would put the server's memory at the mercy of how many documents an
// account happens to have changed since a device was last on wifi.
const MaxDeltaBytes = 8 << 20

// selectDeltaStatement is built once from the registry: one branch per kind, unioned and ordered
// globally so a client receives parents before children within each sequence value.
var selectDeltaStatement = func() string {
	var query strings.Builder
	query.WriteString("WITH delta AS (\n")
	for index, kind := range syncKinds {
		if index > 0 {
			query.WriteString("\tUNION ALL\n")
		}
		fmt.Fprintf(&query,
			"\tSELECT change_seq, %d AS kind_rank, id, %s AS change\n\tFROM %s\n"+
				"\tWHERE account_id = $1::uuid AND change_seq > $2 AND change_seq <= $3\n",
			kind.Rank, kind.changeJSON, kind.Table,
		)
	}
	query.WriteString(")\nSELECT change_seq, change FROM delta ORDER BY change_seq, kind_rank, id LIMIT $4")
	return query.String()
}()

// SelectDelta reads the changes an account accumulated in (sinceCursor, upperBound].
//
// The upper bound is not an optimisation. A caller that read the cursor first and then queried
// without it would miss a push that committed in between: the cursor it returns would sit above
// rows the client never received, and the client would never ask for them again.
// It also stops early once the rows read pass [MaxDeltaBytes], reporting that it did so. A row
// count stopped bounding a response the moment `page_content` joined the registry: 512 documents is
// a number, not a size, and a client asking for a page of them could ask the server to hold
// hundreds of megabytes in memory to answer once (R1).
func SelectDelta(
	ctx context.Context,
	database Querier,
	accountID string,
	sinceCursor int64,
	upperBound int64,
	limit int,
) (delta []DeltaRow, stoppedForBytes bool, err error) {
	rows, err := database.Query(ctx, selectDeltaStatement, accountID, sinceCursor, upperBound, limit)
	if err != nil {
		return nil, false, fmt.Errorf("reading a change delta: %w", err)
	}
	defer rows.Close()

	delta = make([]DeltaRow, 0, min(limit, 512))
	spent := 0
	for rows.Next() {
		var row DeltaRow
		if err := rows.Scan(&row.ChangeSeq, &row.Change); err != nil {
			return nil, false, fmt.Errorf("reading a change delta: %w", err)
		}

		// Over budget, but only ever stopping where one sequence value ends and the next begins:
		// SD2 makes a sequence a batch boundary, and a response that ended inside one would let a
		// client acknowledge half of a push and never see the rest. A single push that exceeds the
		// budget by itself is therefore returned whole — bounded by the push cap, which is what
		// bounds this in the end.
		if spent >= MaxDeltaBytes && len(delta) > 0 && row.ChangeSeq != delta[len(delta)-1].ChangeSeq {
			return delta, true, nil
		}

		spent += len(row.Change)
		delta = append(delta, row)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("reading a change delta: %w", err)
	}
	return delta, false, nil
}

// FindAppliedBatchResponse returns the stored response for a batch the device already pushed.
func FindAppliedBatchResponse(
	ctx context.Context,
	database Querier,
	deviceID string,
	batchID string,
	replayWindow time.Duration,
) (json.RawMessage, bool, error) {
	const statement = `
		SELECT response
		FROM applied_batches
		WHERE device_id = $1::uuid AND batch_id = $2::uuid AND created_at > now() - $3::interval`

	var response json.RawMessage
	err := database.QueryRow(ctx, statement, deviceID, batchID, replayWindow).Scan(&response)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("looking up a batch response: %w", err)
	}
	return response, true, nil
}

// RecordAppliedBatchResponse stores what a push answered, so a retry of the same batch id replays
// it instead of applying the work twice.
//
// Expired rows for the same device are removed in the same statement pair rather than by a
// background job: the set is small, the delete is indexed by the primary key's leading column, and
// a sweeper is one more thing that can be forgotten in a self-hosted deployment.
func RecordAppliedBatchResponse(
	ctx context.Context,
	transaction pgx.Tx,
	deviceID string,
	batchID string,
	response []byte,
	replayWindow time.Duration,
) error {
	const insertStatement = `
		INSERT INTO applied_batches (device_id, batch_id, response)
		VALUES ($1::uuid, $2::uuid, $3::jsonb)
		ON CONFLICT (device_id, batch_id) DO UPDATE SET response = EXCLUDED.response, created_at = now()`

	if _, err := transaction.Exec(ctx, insertStatement, deviceID, batchID, string(response)); err != nil {
		return fmt.Errorf("recording a batch response: %w", err)
	}

	const pruneStatement = `
		DELETE FROM applied_batches
		WHERE device_id = $1::uuid AND created_at < now() - $2::interval`

	if _, err := transaction.Exec(ctx, pruneStatement, deviceID, replayWindow); err != nil {
		return fmt.Errorf("pruning batch responses: %w", err)
	}
	return nil
}

// RecordAcknowledgedSeq stores the highest cursor a device has presented as `since` on a later
// pull. That is stronger than recording the cursor the server is about to return: a response can be
// lost after the database update but before the client receives its body. A cursor presented by the
// client proves it received and committed every change through that sequence, which is what tells
// the purge log that a device has been given the news and the row can go (store.PrunePurges).
//
// It never moves backwards, so an out-of-order request cannot make a device look staler than it is.
func RecordAcknowledgedSeq(ctx context.Context, database Querier, deviceID string, cursor int64) error {
	const statement = `UPDATE devices SET last_pulled_seq = $2 WHERE id = $1::uuid AND last_pulled_seq < $2`

	if _, err := database.Exec(ctx, statement, deviceID, cursor); err != nil {
		return fmt.Errorf("recording an acknowledged pull cursor: %w", err)
	}
	return nil
}
