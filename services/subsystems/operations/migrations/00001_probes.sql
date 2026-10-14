-- +goose Up
CREATE TABLE probes (
    id           text PRIMARY KEY,
    status       text NOT NULL CHECK (status IN ('requested', 'completed')),
    requested_by text NOT NULL,
    hops         jsonb NOT NULL DEFAULT '[]',
    requested_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz
);

-- +goose Down
DROP TABLE probes;
