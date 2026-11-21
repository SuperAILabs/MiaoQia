-- +goose Up
CREATE TABLE sea_public_delivery (
    topic text NOT NULL,
    kafka_partition integer NOT NULL CHECK (kafka_partition >= 0),
    kafka_offset bigint NOT NULL CHECK (kafka_offset >= 0 AND kafka_offset < 9223372036854775807),
    value_sha256 bytea NOT NULL CHECK (octet_length(value_sha256) = 32),
    outcome text NOT NULL CHECK (outcome IN ('journaled', 'verified_noop', 'skipped', 'dead_lettered', 'conflict')),
    journal_seq bigint REFERENCES sea_public_event (seq),
    PRIMARY KEY (topic, kafka_partition, kafka_offset),
    CHECK ((outcome = 'journaled') = (journal_seq IS NOT NULL))
);

-- +goose Down
DROP TABLE sea_public_delivery;
