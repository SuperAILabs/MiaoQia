-- +goose Up
CREATE TABLE sea_public_event (
    seq bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id text NOT NULL UNIQUE,
    envelope jsonb NOT NULL
);

CREATE TABLE sea_public_generation (
    name text PRIMARY KEY,
    basis jsonb NOT NULL,
    final_batch_digest text NOT NULL DEFAULT '',
    applied_seq bigint NOT NULL DEFAULT 0,
    pending_after text NOT NULL DEFAULT ''
);

-- +goose Down
DROP TABLE sea_public_generation;
DROP TABLE sea_public_event;
