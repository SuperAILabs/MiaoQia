"""Synthetic TCP peer and a surviving client with one persistent connection."""

import json
import socket
import socketserver
import sys


def emit(kind, **fields):
    sys.stdout.write(json.dumps({"kind": kind, **fields}, sort_keys=True) + "\n")
    sys.stdout.flush()


def peer(address, client_address):
    class Handler(socketserver.StreamRequestHandler):
        def handle(self):
            emit("connection", expected_peer=self.request.getsockname() == (address, 443),
                 expected_client=self.client_address[0] == client_address)
            for message in self.rfile:
                tag = message.rstrip(b"\n")
                emit("challenge", phase=tag.decode() if tag in {b"before", b"again", b"after"} else "unexpected")
                self.wfile.write(b"ok\n")

    class Server(socketserver.ThreadingTCPServer):
        daemon_threads = True

        def handle_error(self, _request, _address):
            emit("peer_error")

    with Server((address, 443), Handler) as server:
        emit("ready")
        server.serve_forever()


def client(address):
    persistent = None
    emit("ready")
    try:
        for line in sys.stdin:
            stage = line.strip()
            if stage in {"before", "again", "after"}:
                if stage == "before":
                    persistent = socket.create_connection((address, 443), timeout=0.8)
                established = persistent.getsockopt(socket.IPPROTO_TCP, socket.TCP_INFO, 1) == b"\x01"
                persistent.sendall(stage.encode() + b"\n")
                try:
                    reply = persistent.recv(8) == b"ok\n"
                except TimeoutError:
                    emit(stage, timed_out=True, established=established)
                else:
                    emit(stage, acknowledged=reply, established=established)
            elif stage in {"new", "live_new"}:
                try:
                    with socket.create_connection((address, 443), timeout=0.8):
                        emit(stage, connected=True)
                except TimeoutError:
                    emit(stage, timed_out=True)
            else:
                raise ValueError("unknown probe phase")
    finally:
        if persistent is not None:
            persistent.close()


if __name__ == "__main__":
    if sys.argv[1] == "peer":
        peer(*sys.argv[2:])
    elif sys.argv[1] == "client":
        client(*sys.argv[2:])
