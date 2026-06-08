#!/usr/bin/env python3
# Cooperative-app fixture for end-to-end testing of the matrix-tunnel
# proxy + HMAC identity contract.
#
# Exposes the three routes a cooperative web app must support to be
# fully testable from outside:
#
#   GET /              — landing; returns a small HTML page so a browser
#                        sees a 200 + visible content.
#   GET /lan-only/secret
#                      — kiosk-style endpoint. Outpost's `lan_only_paths`
#                        gate should 404 this when the request arrives
#                        through the matrix tunnel (X-Forwarded-Prefix
#                        present). Direct LAN GET should still see it.
#   GET /echo-headers  — returns JSON of the request headers + URL the
#                        upstream observed. The E2E asserts that the
#                        cloudbox-stamped identity headers
#                        (X-Periscope-User, X-Periscope-Role,
#                        X-Forwarded-Prefix) and the optional HMAC
#                        (X-Periscope-Signature, X-Periscope-Timestamp)
#                        all reach the upstream verbatim.
#
# Language-neutral by design — pure stdlib, no third-party packages, so
# it runs on any host with python3.

import json
import os
import sys
from http.server import HTTPServer, BaseHTTPRequestHandler


PORT = int(os.environ.get("PORT", "58080"))


class Handler(BaseHTTPRequestHandler):
    def _respond(self, code, body, content_type="text/plain; charset=utf-8"):
        if isinstance(body, str):
            body = body.encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = self.path.split("?", 1)[0]
        if path == "/":
            self._respond(
                200,
                "<html><body><h1>Cooperative app fixture</h1>"
                "<p>Reachable through the matrix tunnel.</p></body></html>",
                "text/html; charset=utf-8",
            )
            return
        if path.startswith("/lan-only/"):
            # Outpost's lan_only_paths gate should 404 this when the
            # request came in through cloudbox. If we see it here, the
            # gate was bypassed.
            self._respond(
                200,
                json.dumps({"reached": path, "note": "gate-bypassed-if-seen-from-tunnel"}),
                "application/json",
            )
            return
        if path == "/echo-headers":
            headers = {k: v for k, v in self.headers.items()}
            body = json.dumps(
                {
                    "method": self.command,
                    "path": self.path,
                    "headers": headers,
                },
                indent=2,
                sort_keys=True,
            )
            self._respond(200, body, "application/json")
            return
        self._respond(404, json.dumps({"error": "not_found", "path": path}), "application/json")

    def log_message(self, *args, **kwargs):
        # Quiet by default. CI surfaces failures from the test driver;
        # this server's noise just clutters logs.
        return


def main():
    server = HTTPServer(("0.0.0.0", PORT), Handler)
    sys.stderr.write(f"cooperative-app fixture listening on :{PORT}\n")
    server.serve_forever()


if __name__ == "__main__":
    main()
