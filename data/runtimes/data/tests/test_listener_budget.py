import time

import pytest

from miaoqia.kernel import db, reqctx
from miaoqia.runtime_data import POOL_TIMEOUT

# Port 9 is never a PostgreSQL server; reaching the connect attempt is enough.
UNREACHABLE = "postgres://nobody:nobody@127.0.0.1:9/none"
CTX = reqctx.Context(reqctx.Principal("svc:detection", "service"), "tnt_a", "cell-01", "monitoring")


def test_listener_pool_checkout_fits_inside_request_budget():
    budget = 5.0  # internal_http.Server default and maximum per-request budget
    with reqctx.use(CTX):
        # The listener assembly reaches the connection attempt within a full budget.
        with pytest.raises(Exception) as raised:
            with db.Database(UNREACHABLE, pool_timeout=POOL_TIMEOUT).tx(deadline=time.monotonic() + budget):
                pass
        assert not isinstance(raised.value, TimeoutError)
        # The default checkout bound equals the budget and rejects every request.
        with pytest.raises(TimeoutError):
            with db.Database(UNREACHABLE).tx(deadline=time.monotonic() + budget):
                pass
