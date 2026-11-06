import os
from urllib.parse import urlsplit
import psycopg
import pytest
from miaoqia.acquisition import MIGRATIONS
from miaoqia.acquisition.infrastructure.privileges import provision
from miaoqia.acquisition.infrastructure.rights import Registry
from miaoqia.kernel import db


@pytest.fixture
def registry(pg_factory):
    dsns = pg_factory()
    db.migrate(dsns["owner"], MIGRATIONS)
    reader = urlsplit(dsns["app"]).username
    operator = reader + "_op"
    with psycopg.connect(os.environ["MIAOQIA_TEST_PG"], autocommit=True) as c:
        c.execute(f"CREATE ROLE {operator} LOGIN PASSWORD 'pw' NOBYPASSRLS")
    opdsn = dsns["app"].replace(reader + ":", operator + ":")
    provision(dsns["owner"], reader, operator)
    read, write = Registry(dsns["app"]), Registry(opdsn)
    yield read, write, {
        **dsns,
        "operator": opdsn,
        "reader_role": reader,
        "operator_role": operator,
    }
    read.close()
    write.close()
    with psycopg.connect(
        os.environ["MIAOQIA_TEST_PG"],
        dbname=urlsplit(dsns["owner"]).path[1:],
        autocommit=True,
    ) as c:
        c.execute(f"DROP OWNED BY {operator}")
    with psycopg.connect(os.environ["MIAOQIA_TEST_PG"], autocommit=True) as c:
        c.execute(f"DROP ROLE {operator}")
