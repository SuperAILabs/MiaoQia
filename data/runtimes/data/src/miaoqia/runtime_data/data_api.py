"""data-api 的最小内部读取服务。

跨语言调用已经有稳定的 Go transport（``services/kernel/dataread``）；本模块只负责
把同一组读取操作绑定到 Python 数据平面的本地查询对象。生产模式要求内部 mTLS，开发
模式允许 HTTP，和 platform 的 transportsecurity 保持同一边界。
"""
from __future__ import annotations

import dataclasses
import datetime as dt
import json
import os
import signal
import ssl
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from threading import Event
from typing import Any

from miaoqia.domain_intelligence.api import Basis, Facts
from miaoqia.evidence.api import EvidenceResolver
from miaoqia.kernel import reqctx

CONTEXT_HEADER = "Miaoqia-Context"
MAX_REQUEST_BYTES = 1 << 20


def _iso(value: dt.datetime | None) -> str | None:
    return value.isoformat().replace("+00:00", "Z") if value is not None else None


def _basis(value: Any) -> Basis:
    if not isinstance(value, dict):
        raise ValueError("basis 必须为对象")
    cutoff = value.get("recorded_cutoff")
    if cutoff is not None:
        if not isinstance(cutoff, str):
            raise ValueError("recorded_cutoff 必须为字符串")
        cutoff = dt.datetime.fromisoformat(cutoff.replace("Z", "+00:00"))
    return Basis(view=value.get("view"), recorded_cutoff=cutoff)


def _basis_json(value: Basis) -> dict[str, Any]:
    return {"view": value.view, "recorded_cutoff": _iso(value.recorded_cutoff)}


def _jsonable(value: Any) -> Any:
    if dataclasses.is_dataclass(value):
        return _jsonable(dataclasses.asdict(value))
    if isinstance(value, dt.datetime):
        return _iso(value)
    if isinstance(value, dict):
        return {str(k): _jsonable(v) for k, v in value.items()}
    if isinstance(value, (tuple, list)):
        return [_jsonable(v) for v in value]
    return value


def _page(value: Any) -> dict[str, Any]:
    return {"basis": _basis_json(value.basis), "items": _jsonable(value.items), "next_cursor": value.next_cursor}


class DataAPIHandler(BaseHTTPRequestHandler):
    server: "DataAPIServer"

    def log_message(self, _format: str, *_args: Any) -> None:
        return

    def do_GET(self) -> None:  # noqa: N802
        if self.path == "/healthz":
            self._json(HTTPStatus.OK, {"status": "ok"})
            return
        self._problem(HTTPStatus.NOT_FOUND, "not_found", "未知路径")

    def do_POST(self) -> None:  # noqa: N802
        if not self.server.peer_allowed(self):
            self._problem(HTTPStatus.UNAUTHORIZED, "internal_peer_invalid", "内部调用方未通过身份验证")
            return
        try:
            encoded_context = self.headers.get(CONTEXT_HEADER, "")
            if not encoded_context:
                self._problem(HTTPStatus.UNAUTHORIZED, "context_missing", "缺少内部请求上下文")
                return
            try:
                context = reqctx.Context.decode(encoded_context)
            except (TypeError, ValueError) as exc:
                self._problem(HTTPStatus.UNAUTHORIZED, "context_invalid", str(exc))
                return
            with reqctx.use(context):
                body = self._body()
                result = self.server.dispatch(self.path, body)
            self._json(HTTPStatus.OK, _jsonable(result))
        except KeyError as exc:
            self._problem(HTTPStatus.BAD_REQUEST, "invalid_request", f"缺少字段：{exc.args[0]}")
        except (TypeError, ValueError) as exc:
            self._problem(HTTPStatus.BAD_REQUEST, "invalid_request", str(exc))
        except PermissionError as exc:
            self._problem(HTTPStatus.SERVICE_UNAVAILABLE, "rights_unavailable", str(exc))
        except LookupError as exc:
            self._problem(HTTPStatus.NOT_FOUND, "content_unavailable", str(exc))
        except Exception as exc:  # noqa: BLE001 — 内部读取失败必须失败关闭
            self._problem(HTTPStatus.SERVICE_UNAVAILABLE, "data_unavailable", str(exc))

    def _body(self) -> dict[str, Any]:
        length = int(self.headers.get("Content-Length", "-1"))
        if length < 0 or length > MAX_REQUEST_BYTES:
            raise ValueError("请求体超过大小限制")
        raw = self.rfile.read(length)
        body = json.loads(raw)
        if not isinstance(body, dict):
            raise ValueError("请求体必须为对象")
        return body

    def _json(self, status: HTTPStatus, value: Any) -> None:
        raw = json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def _problem(self, status: HTTPStatus, code: str, detail: str) -> None:
        self._json(status, {"code": code, "detail": detail})


class DataAPIServer(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, address: tuple[str, int], facts: Facts, evidence: EvidenceResolver, evidence_object=None):
        super().__init__(address, DataAPIHandler)
        self.facts, self.evidence, self.evidence_object = facts, evidence, evidence_object

    def dispatch(self, path: str, body: dict[str, Any]) -> Any:
        basis = _basis(body.get("basis", {"view": "current"}))
        if path == "/internal/v1/data/get-fact":
            value = self.facts.get_fact(body["fact_id"], basis, version=body.get("version"))
            if value is None:
                raise LookupError("事实不存在或不可见")
            return {"basis": _basis_json(value.basis), "fact": value.fact}
        if path == "/internal/v1/data/fact-history":
            return _page(self.facts.get_fact_history(body["fact_id"], basis, after=body.get("after", 0), limit=body.get("limit", 50)))
        if path == "/internal/v1/data/query-facts":
            return _page(self.facts.query_facts(body["info_domain"], body["object_type"], basis,
                                                after=body.get("after", ""), limit=body.get("limit", 50)))
        if path == "/internal/v1/data/bid-result-units":
            return _page(self.facts.bid_result_units(body["fact_id"], basis, version=body.get("version")))
        if path == "/internal/v1/data/resolve-evidence":
            value = self.evidence.resolve(body["evidence_ref"])
            if value is None:
                raise LookupError("证据不存在或不可见")
            out = _jsonable(value)
            if self.evidence_object is not None:
                out["object_id"] = self.evidence_object(body["evidence_ref"])
            return out
        raise LookupError("未知 data-api 操作")

    @staticmethod
    def peer_allowed(handler: BaseHTTPRequestHandler) -> bool:
        if os.environ.get("MIAOQIA_ENV", "") == "development":
            return True
        if not hasattr(handler.connection, "getpeercert"):
            return False
        cert = handler.connection.getpeercert()
        uris = [value for kind, value in cert.get("subjectAltName", ()) if kind == "URI"]
        allowed = {item.strip() for item in os.environ.get("MIAOQIA_INTERNAL_ALLOWED_PEERS", "").split(",") if item.strip()}
        return len(uris) == 1 and uris[0].startswith("spiffe://miaoqia/service/") and uris[0] in allowed


def serve(address: str, facts: Facts, evidence: EvidenceResolver, evidence_object=None) -> None:
    host, raw_port = address.rsplit(":", 1)
    server = DataAPIServer((host or "0.0.0.0", int(raw_port)), facts, evidence, evidence_object=evidence_object)
    server.timeout = 1.0
    if os.environ.get("MIAOQIA_ENV", "") != "development":
        context = ssl.create_default_context(ssl.Purpose.CLIENT_AUTH)
        context.minimum_version = ssl.TLSVersion.TLSv1_3
        context.load_cert_chain(os.environ["MIAOQIA_INTERNAL_TLS_CERT"], os.environ["MIAOQIA_INTERNAL_TLS_KEY"])
        context.load_verify_locations(cafile=os.environ["MIAOQIA_INTERNAL_TLS_CA"])
        context.verify_mode = ssl.CERT_REQUIRED
        server.socket = context.wrap_socket(server.socket, server_side=True)
    stop = Event()
    signal.signal(signal.SIGTERM, lambda *_: stop.set())
    signal.signal(signal.SIGINT, lambda *_: stop.set())
    while not stop.is_set():
        server.handle_request()
    server.server_close()
