-- +goose Up

-- `last_pulled_seq` used to be filled with the cursor the server was about to return. That was a
-- useful diagnostic, but not an acknowledgement: the connection could disappear after this value
-- was written and before the response reached the device. Permanent tombstone deletion needs the
-- stronger meaning below, so reset existing values once. Every active device will repopulate its
-- durable cursor on its next ordinary pull by presenting it as `since`.
UPDATE devices SET last_pulled_seq = 0;

COMMENT ON COLUMN devices.last_pulled_seq IS
    'Highest pull cursor later presented by the device as since, acknowledging local commit';

-- +goose Down

-- The reset is intentionally not reversible: the previous values were not acknowledgements and
-- putting them back would make a rolled-back server trust them as if they were.
COMMENT ON COLUMN devices.last_pulled_seq IS
    'Diagnostic cursor last reported by the device';
