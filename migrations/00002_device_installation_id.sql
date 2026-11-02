-- +goose Up

-- The app generates one id per installation and sends it with every password sign-in. With it, a
-- sign-in from an installation that already has a device row rotates that row's token instead of
-- adding a second row for the same tablet — which is what every re-login did before, leaving the
-- device list growing one stale entry per sign-in.
--
-- Nullable because the id is optional on the wire: an older or third-party client that sends none
-- gets a new row per sign-in, as it always did. The index is partial for the same reason; NULLs
-- never conflict.
ALTER TABLE devices ADD COLUMN installation_id uuid;

CREATE UNIQUE INDEX devices_account_installation_key
    ON devices (account_id, installation_id)
    WHERE installation_id IS NOT NULL;

-- +goose Down

DROP INDEX devices_account_installation_key;
ALTER TABLE devices DROP COLUMN installation_id;
