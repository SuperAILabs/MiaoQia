"""Live observation cursor and proven aliases to pre-existing evidence streams."""
from alembic import op

revision = "0004"
down_revision = "0003"
branch_labels = None
depends_on = None


def upgrade():
    op.execute("""
CREATE TABLE evd_live_observations (
    source_id text NOT NULL,
    notice_key text NOT NULL CHECK (notice_key <> ''),
    stream_key text NOT NULL CHECK (stream_key <> ''),
    version bigint NOT NULL CHECK (version > 0),
    PRIMARY KEY (source_id, notice_key),
    UNIQUE (source_id, stream_key)
);
CREATE TABLE evd_live_urls (
    source_id text NOT NULL,
    source_url text NOT NULL,
    PRIMARY KEY (source_id, source_url)
);
""")


def downgrade():
    op.execute("DROP TABLE evd_live_urls, evd_live_observations")
