-- +goose Up

-- The shelf a notebook sits on, as two nullable client wall clocks
-- (viveNotes/memory/closedNotebooksPlan.md, schema 22).
--
-- `closed_at` says the notebook is off the rail. `cloud_only_at` says its bodies, ink and pictures
-- are held here and not on the device. A cloud-only notebook is always also closed, but the two
-- answer different questions, which is why they are two columns rather than one state on the first.
--
-- Typed columns rather than the `extra` escape hatch they have been landing in since the app
-- shipped them. `extra` is what keeps an older server from dropping a newer client's field; it is
-- not where a field lives once the server has to act on it. This server now does: it refuses to
-- turn a closed notebook's delete into a tombstone, and the admin panel divides its notebook list
-- by where the bytes are. Both are predicates, and a predicate over jsonb is one nobody can index
-- or read at a glance.
ALTER TABLE notebooks
    ADD COLUMN closed_at     bigint,
    ADD COLUMN cloud_only_at bigint;

COMMENT ON COLUMN notebooks.closed_at IS
    'Client wall clock at which the notebook left the rail, or null while it is open';
COMMENT ON COLUMN notebooks.cloud_only_at IS
    'Client wall clock from which this server holds the only copy of the contents, or null';

-- Values a client already pushed while this server treated them as unrecognised. They were stored
-- verbatim and returned verbatim, so they are correct; they are simply in the wrong place. Lifting
-- them is not optional housekeeping: `extra ||` is the first term of the rendered change, so a copy
-- left behind would be shadowed by the column on every pull and would then sit there for ever,
-- disagreeing with it and answering nothing.
--
-- `jsonb_typeof` guards the cast. The app writes a number or JSON null, but `extra` is by
-- definition whatever some client sent, and a migration that fails on one malformed row would take
-- the whole server down on startup.
UPDATE notebooks
SET closed_at = CASE
        WHEN jsonb_typeof(extra -> 'closedAt') = 'number' THEN (extra ->> 'closedAt')::bigint
    END,
    cloud_only_at = CASE
        WHEN jsonb_typeof(extra -> 'cloudOnlyAt') = 'number' THEN (extra ->> 'cloudOnlyAt')::bigint
    END,
    extra = extra - 'closedAt' - 'cloudOnlyAt'
WHERE extra ?| ARRAY['closedAt', 'cloudOnlyAt'];

-- +goose Down

-- Put the values back where an older build looks for them, so rolling the server back does not
-- reopen every closed notebook on every device the next time one pulls.
UPDATE notebooks
SET extra = extra
        || CASE WHEN closed_at IS NULL THEN '{}'::jsonb
                ELSE jsonb_build_object('closedAt', closed_at) END
        || CASE WHEN cloud_only_at IS NULL THEN '{}'::jsonb
                ELSE jsonb_build_object('cloudOnlyAt', cloud_only_at) END
WHERE closed_at IS NOT NULL OR cloud_only_at IS NOT NULL;

ALTER TABLE notebooks
    DROP COLUMN closed_at,
    DROP COLUMN cloud_only_at;
