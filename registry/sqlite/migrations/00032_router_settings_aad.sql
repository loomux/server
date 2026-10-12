-- LOOM-189: a router tier's API key is bound to the provider and base URL
-- it was set for as well as its tier: the AES-GCM additional data names
-- all three, so a row whose provider or base_url is changed behind the
-- store's back no longer decrypts (the key can't be redirected).
-- seal_version says what a row is bound to: 1 its tier (LOOM-185), 2 its
-- tier, provider and base URL. Existing rows keep 1; Open re-seals them
-- once a master key is configured (Store.resealRouterTiers) and records
-- the re-seal as done in router_settings_reseal, so a row set back to 1
-- later is never re-sealed.
-- +goose Up
ALTER TABLE router_settings ADD COLUMN seal_version INTEGER NOT NULL DEFAULT 1;

CREATE TABLE router_settings_reseal (
    seal_version INTEGER PRIMARY KEY,
    done_at      TIMESTAMP NOT NULL
);

-- +goose Down
-- Code from before this migration can't read a row bound to its provider
-- and base URL: it shows the tier as unreadable and uses the
-- environment's settings until the tier is saved again in Settings.
DROP TABLE router_settings_reseal;
ALTER TABLE router_settings DROP COLUMN seal_version;
