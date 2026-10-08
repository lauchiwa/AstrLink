"""Unit/integration checks for the test-only TLS transport, not WebView admission."""

import http.client
import io
import json
import os
from pathlib import Path
import shutil
import socket
import socketserver
import ssl
import subprocess
import sys
import tempfile
import threading
import unittest

import tls_fixture


class RequestBoundaryTests(unittest.TestCase):
    def test_only_fixed_bodyless_gets_and_the_fixture_authority_are_allowed(self):
        for path in tls_fixture.PATHS:
            request = f"GET {path} HTTP/1.1\r\nHost: localhost:443\r\n\r\n".encode()
            self.assertTrue(tls_fixture.request_allowed(request, "localhost:443"))
        for request in (
            b"POST /seed/one HTTP/1.1\r\nHost: localhost:443\r\n\r\n",
            b"GET https://other.test/ HTTP/1.1\r\nHost: localhost:443\r\n\r\n",
            b"GET /seed/one HTTP/1.1\r\nHost: other.test\r\n\r\n",
            b"GET /seed/one HTTP/1.1\r\nHost: localhost:443\r\nHost: localhost:443\r\n\r\n",
            b"GET /seed/one HTTP/1.1\r\nHost: localhost:443\r\nContent-Length: 0\r\n\r\n",
            b"GET /seed/one HTTP/1.1\r\nHost: localhost:443\r\nTransfer-Encoding: chunked\r\n\r\n",
            b"GET /seed/one HTTP/1.1\r\nHost: localhost:443\r\nAuthorization: synthetic\r\n\r\n",
            b"GET /seed/one HTTP/1.1\r\nHost: localhost:443\r\nProxy-Authorization: synthetic\r\n\r\n",
            b"GET /seed/one HTTP/1.1\r\nHost: localhost:443\r\n\xff: x\r\n\r\n",
            b"x" * (tls_fixture.LIMIT + 1),
        ):
            self.assertFalse(tls_fixture.request_allowed(request, "localhost:443"))

    def test_configuration_rejects_nonfiles_urls_ports_and_large_input(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "fixture"
            path.write_bytes(b"synthetic")
            config = dict(host="localhost", backend_port=1234,
                          certificate=str(path), key=str(path))
            self.assertEqual(tls_fixture.configuration(
                io.BytesIO(json.dumps(config).encode() + b"\n")), config)
            for changes in (
                {"host": "user@localhost"}, {"host": "https://localhost"},
                {"host": "localhost:443"}, {"backend_port": 0},
                {"backend_port": True}, {"backend_port": 65536},
                {"key": directory},
            ):
                value = dict(config, **changes)
                with self.assertRaises(ValueError):
                    tls_fixture.configuration(io.BytesIO(json.dumps(value).encode() + b"\n"))
        with self.assertRaises(ValueError):
            tls_fixture.configuration(io.BytesIO(b"x" * (tls_fixture.LIMIT + 1)))


class TlsTransportTests(unittest.TestCase):
    @unittest.skipUnless(shutil.which("openssl"), "openssl required for ephemeral test certificate")
    def test_tls_verification_request_scope_and_parent_eof_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            cert = Path(directory) / "certificate.pem"
            key = Path(directory) / "key.pem"
            # This root is trusted only by the test's Python client below. It
            # is never installed into the OS or used to override WebKit trust.
            subprocess.run([
                "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
                "-days", "1", "-subj", "/CN=localhost", "-addext", "subjectAltName=DNS:localhost",
                "-keyout", str(key), "-out", str(cert),
            ], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15)
            os.chmod(key, 0o600)
            forwarded = []

            class Backend(socketserver.BaseRequestHandler):
                def handle(self):
                    self.request.settimeout(2)
                    request = bytearray()
                    while not request.endswith(b"\r\n\r\n"):
                        part = self.request.recv(8192)
                        if not part:
                            return
                        request.extend(part)
                    forwarded.append(bytes(request))
                    self.request.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}")

            with socketserver.TCPServer(("127.0.0.1", 0), Backend) as backend:
                worker = threading.Thread(target=backend.serve_forever, daemon=True)
                worker.start()
                process = subprocess.Popen([
                    sys.executable, "-I", "-u", str(Path(tls_fixture.__file__).resolve())
                ], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                try:
                    config = dict(host="localhost", backend_port=backend.server_address[1],
                                  certificate=str(cert), key=str(key))
                    process.stdin.write(json.dumps(config).encode() + b"\n")
                    process.stdin.flush()
                    port = json.loads(process.stdout.readline())["port"]
                    with self.assertRaises(ssl.SSLCertVerificationError):
                        with socket.create_connection(("127.0.0.1", port), timeout=2) as raw:
                            ssl.create_default_context().wrap_socket(raw, server_hostname="localhost")
                    trusted = ssl.create_default_context(cafile=str(cert))
                    with self.assertRaises(ssl.SSLCertVerificationError):
                        with socket.create_connection(("127.0.0.1", port), timeout=2) as raw:
                            trusted.wrap_socket(raw, server_hostname="wrong.invalid")
                    connection = http.client.HTTPSConnection("localhost", port, context=trusted, timeout=2)
                    connection.request("GET", "/seed/one")
                    response = connection.getresponse()
                    self.assertEqual(response.status, 200)
                    self.assertEqual(response.read(), b"{}")
                    connection.close()
                    connection = http.client.HTTPSConnection("localhost", port, context=trusted, timeout=2)
                    connection.request("GET", "/not-allowed")
                    response = connection.getresponse()
                    self.assertEqual(response.status, 400)
                    response.read()
                    connection.close()
                    process.stdin.close()
                    self.assertEqual(process.wait(timeout=5), 0)
                    observations = json.loads(process.stdout.readline())
                    self.assertEqual(observations, dict(handshake_failures=2, requests=1, rejected_requests=1))
                    self.assertEqual(len(forwarded), 1)
                    self.assertEqual(process.stderr.read(), b"")
                finally:
                    if process.poll() is None:
                        process.kill()
                        process.wait(timeout=5)
                    for stream in (process.stdin, process.stdout, process.stderr):
                        stream.close()
                    backend.shutdown()
                    worker.join(timeout=3)


if __name__ == "__main__":
    unittest.main()
