#!/usr/bin/env python3
"""Development web server for the web UI (make serve / make serve-https).

Serves the repo root on all interfaces of the lab LAN. Web Serial/WebUSB need
a secure context: http:// works only as http://localhost on this machine;
other machines need --https (self-signed certificate, accept the warning once).
"""
import argparse
import functools
import json
import http.server
import os
import socket
import ssl
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import version_info  # noqa: E402

ROOT = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
CERT_DIR = os.path.join(ROOT, "bench-out", "serve-cert")  # bench-out/ is git-ignored


def lan_addresses():
    addrs = set()
    try:
        out = subprocess.run(["ip", "-4", "-o", "addr", "show", "scope", "global"], capture_output=True, text=True, check=True).stdout
        for line in out.splitlines():
            addrs.add(line.split()[3].split("/")[0])
    except Exception:
        try:
            addrs.add(socket.gethostbyname(socket.gethostname()))
        except OSError:
            pass
    return sorted(addrs)


def ensure_cert(addrs):
    crt, key = os.path.join(CERT_DIR, "cert.pem"), os.path.join(CERT_DIR, "key.pem")
    san = ",".join(["DNS:localhost", f"DNS:{socket.gethostname()}", "IP:127.0.0.1"] + [f"IP:{a}" for a in addrs])
    stamp = os.path.join(CERT_DIR, "san.txt")
    if os.path.exists(crt) and os.path.exists(stamp) and open(stamp).read() == san:
        return crt, key
    os.makedirs(CERT_DIR, exist_ok=True)
    subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "825", "-keyout", key, "-out", crt,
                    "-subj", "/CN=capturetonkeli lab", "-addext", f"subjectAltName={san}"], check=True, capture_output=True)
    with open(stamp, "w") as f:
        f.write(san)
    print(f"created self-signed certificate for {san}", file=sys.stderr)
    return crt, key


class Handler(http.server.SimpleHTTPRequestHandler):
    extensions_map = {**http.server.SimpleHTTPRequestHandler.extensions_map,
                      ".js": "text/javascript", ".mjs": "text/javascript", ".webmanifest": "application/manifest+json", ".svg": "image/svg+xml"}

    def end_headers(self):
        self.send_header("Cache-Control", "no-cache")  # always the latest files while developing
        super().end_headers()

    def do_GET(self):
        # version.json: generated live from git (on Pages the deploy workflow writes it), docs/04 §19
        if self.path.split("?")[0] in ("/version.json", "/capturetonkeli/version.json"):
            body = json.dumps(version_info.info("dev-server")).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        super().do_GET()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=8000)
    ap.add_argument("--bind", default="0.0.0.0")
    ap.add_argument("--https", action="store_true")
    a = ap.parse_args()
    addrs = lan_addresses()
    srv = http.server.ThreadingHTTPServer((a.bind, a.port), functools.partial(Handler, directory=ROOT))
    scheme = "https" if a.https else "http"
    if a.https:
        crt, key = ensure_cert(addrs)
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.load_cert_chain(crt, key)
        srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
    print(f"Serving {ROOT}")
    print(f"  this machine:  {scheme}://localhost:{a.port}/   (USB works)")
    for ip in addrs:
        print(f"  lab LAN:       {scheme}://{ip}:{a.port}/" + ("   (USB works after accepting the certificate warning)" if a.https
                                                               else "   (page + demo only: USB needs https → make serve-https)"))
    print("Ctrl-C to stop.")
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
