#!/usr/bin/env python3
"""Release-server simulator for the installer tests (tests only, never
shipped in the installer).

Serves:
  /manifest            -> $FIXTURE_DIR/manifest-pretty.json
  /manifest-minified   -> $FIXTURE_DIR/manifest-minified.json
  /releases/download/v<ver>/<asset>  -> $FIXTURE_DIR/assets/<asset>
  /releases/download/v<ver>/SHA256SUMS -> $FIXTURE_DIR/SHA256SUMS-<ver>
  /raw/<ref>/config.toml.example       -> fixture example (any ref accepted;
                                          the ref is echoed in a header and
                                          can be asserted by tests)

Usage: server.py <port> <fixture_dir>
"""
import http.server
import os
import sys
import urllib.parse


def main() -> int:
    port = int(sys.argv[1])
    fixture_dir = sys.argv[2]

    class Handler(http.server.BaseHTTPRequestHandler):
        def _send_file(self, path: str, content_type: str = "application/octet-stream") -> None:
            try:
                with open(path, "rb") as fh:
                    body = fh.read()
            except OSError:
                self.send_error(404, "fixture not found")
                return
            self.send_response(200)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self) -> None:  # noqa: N802 (http.server API)
            parsed = urllib.parse.urlparse(self.path)
            if parsed.path == "/stall":
                # Accept the connection, then delay headers/body to exercise
                # the installer's bounded request timeout.
                import time
                time.sleep(10)
                return
            parts = parsed.path.split("/")
            if parsed.path == "/manifest":
                self._send_file(os.path.join(fixture_dir, "manifest-pretty.json"), "application/json")
                return
            if parsed.path == "/manifest-minified":
                self._send_file(os.path.join(fixture_dir, "manifest-minified.json"), "application/json")
                return
            if parsed.path.startswith("/raw/"):
                # Test seam: a FAIL-RAW marker in the fixture dir simulates
                # a raw-host outage without touching release endpoints.
                if os.path.exists(os.path.join(fixture_dir, "FAIL-RAW")):
                    self.send_error(500, "raw host down (FAIL-RAW)")
                    return
                # /raw/<ref...>/config.toml.example — ref may contain slashes.
                tail = urllib.parse.unquote(parsed.path[len("/raw/"):])
                ref = tail.rsplit("/", 1)[0]
                body = f"# config.toml.example (served for ref: {ref})\n".encode()
                self.send_response(200)
                self.send_header("Content-Type", "text/plain")
                self.send_header("Content-Length", str(len(body)))
                self.send_header("X-Test-Raw-Ref", ref)
                self.end_headers()
                self.wfile.write(body)
                return
            # /releases/download/v<ver>/<name>
            if len(parts) == 5 and parts[1] == "releases" and parts[2] == "download" and parts[3].startswith("v"):
                version = parts[3][1:]
                name = urllib.parse.unquote(parts[4])
                if name == "SHA256SUMS":
                    self._send_file(os.path.join(fixture_dir, f"SHA256SUMS-{version}"))
                else:
                    self._send_file(os.path.join(fixture_dir, "assets", name))
                return
            # /config.toml.example — unversioned raw path (no ref segment).
            if parsed.path == "/config.toml.example":
                self._send_file(os.path.join(fixture_dir, "config.toml.example"), "text/plain")
                return
            self.send_error(404, "not simulated")

        def log_message(self, fmt: str, *args: object) -> None:
            # Keep test output clean; failures surface via 404s/curl errors.
            pass

    server = http.server.ThreadingHTTPServer(("127.0.0.1", port), Handler)
    server.serve_forever()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
