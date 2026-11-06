"""Read-only internal routes; transport/identity admission precedes any registry access."""

from miaoqia.kernel.internal_http import closed


def routes(decisions):
    def resolve(body, deadline):
        return decisions.evaluate(body)

    def confirm(body, deadline):
        return decisions.evaluate(body, confirm=True)

    def status(body, deadline):
        closed(body, set())
        return decisions.status()

    return {
        "/internal/v1/source-rights/resolve": ("rights.resolve", resolve),
        "/internal/v1/source-rights/confirm": ("rights.confirm", confirm),
        "/internal/v1/source-rights/status": ("rights.status", status),
    }
