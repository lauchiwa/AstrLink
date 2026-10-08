"""Loopback-only TLS transport for the standalone Rust isolation probe.

Reads configuration from stdin, never changes trust settings, never downloads
certificates, and exits when its parent's stdin pipe closes. Only fixed synthetic
fixture GETs may reach the one loopback backend. Not a general-purpose proxy.
"""

import json
import socket
import socketserver
import ssl
import sys
import threading
from pathlib import Path

LIMIT = 8192
ACCOUNTS = ("main", "one", "two")
PATHS = frozenset(
    ["/empty"]
    + [f"/{route}/{account}" for route in ("seed", "self", "cache", "http-cache")
       for account in ACCOUNTS]
)


def request_allowed(data, authority):
    if len(data) > LIMIT or not data.endswith(b"\r\n\r\n"):
        return False
    try:
        lines = data.decode("ascii").split("\r\n")
    except UnicodeDecodeError:
        return False
    request = lines[0].split(" ")
    if len(request) != 3 or request[0] != "GET" or request[1] not in PATHS:
        return False
    if request[2] not in ("HTTP/1.0", "HTTP/1.1"):
        return False
    headers = {}
    for line in lines[1:-2]:
        name, colon, value = line.partition(":")
        name = name.lower()
        if not colon or not name or name in headers or name.strip() != name:
            return False
        headers[name] = value.strip()
    return (headers.get("host", "").lower() == authority
            and "transfer-encoding" not in headers
            and "content-length" not in headers
            and "authorization" not in headers
            and "proxy-authorization" not in headers)


def configuration(stream):
    line = stream.readline(LIMIT + 1)
    if len(line) > LIMIT or not line.endswith(b"\n"):
        raise ValueError("invalid configuration")
    config = json.loads(line)
    host = config["host"]
    if (not isinstance(host, str) or not host or len(host) > 253
            or any(char not in "abcdefghijklmnopqrstuvwxyz0123456789.-" for char in host)):
        raise ValueError("invalid host")
    port = config["backend_port"]
    if not isinstance(port, int) or isinstance(port, bool) or not 1 <= port <= 65535:
        raise ValueError("invalid backend")
    for key in ("certificate", "key"):
        path = Path(config[key])
        if not path.is_file() or path.stat().st_size > 65536:
            raise ValueError("invalid certificate input")
    return config


def run(config, stdin):
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.load_cert_chain(config["certificate"], config["key"])
    counts = {"handshake_failures": 0, "requests": 0, "rejected_requests": 0}
    lock = threading.Lock()
    admission = threading.BoundedSemaphore(4)

    def count(key):
        with lock:
            counts[key] += 1

    class Handler(socketserver.BaseRequestHandler):
        def handle(self):
            if not admission.acquire(blocking=False):
                count("rejected_requests")
                return
            try:
                self.request.settimeout(2)
                try:
                    connection = context.wrap_socket(self.request, server_side=True)
                except (ssl.SSLError, OSError):
                    count("handshake_failures")
                    return
                with connection:
                    data = bytearray()
                    while len(data) <= LIMIT and not data.endswith(b"\r\n\r\n"):
                        part = connection.recv(min(1024, LIMIT + 1 - len(data)))
                        if not part:
                            return
                        data.extend(part)
                    authority = f"{config['host']}:{self.server.server_address[1]}"
                    if not request_allowed(data, authority):
                        count("rejected_requests")
                        connection.sendall(b"HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
                        return
                    with socket.create_connection(("127.0.0.1", config["backend_port"]), timeout=2) as backend:
                        backend.sendall(data)
                        response = bytearray()
                        while len(response) <= 65536:
                            part = backend.recv(min(4096, 65537 - len(response)))
                            if not part:
                                break
                            response.extend(part)
                        if len(response) > 65536:
                            return
                    count("requests")
                    connection.sendall(response)
            except (OSError, ValueError):
                count("rejected_requests")
            finally:
                admission.release()

    class Server(socketserver.ThreadingTCPServer):
        daemon_threads = True

        def handle_error(self, _request, _address):
            # Never print request data, credentials, certificate paths or a
            # Python exception that could include them.
            count("rejected_requests")

    with Server(("127.0.0.1", 0), Handler) as server:
        worker = threading.Thread(target=server.serve_forever, daemon=True)
        worker.start()
        print(json.dumps({"port": server.server_address[1]}), flush=True)
        stdin.read()  # EOF is the lifetime fence; no subsequent control input.
        server.shutdown()
        worker.join(timeout=3)
    print(json.dumps(counts), flush=True)


if __name__ == "__main__":
    try:
        run(configuration(sys.stdin.buffer), sys.stdin.buffer)
    except Exception:
        print(json.dumps({"error": "tls_fixture_failed"}), flush=True)
        sys.exit(1)
