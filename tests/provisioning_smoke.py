#!/usr/bin/env python3
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Exercise real bridge startup/authentication with a local mock homeserver.

Run after building: python3 tests/provisioning_smoke.py --binary ./mautrix-discord
Requires PyYAML. No Discord login or external homeserver is used.
"""
import argparse
import http.server
import json
import pathlib
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request

import yaml


class Homeserver(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def reply(self):
        size = int(self.headers.get("Content-Length", "0"))
        if size:
            self.rfile.read(size)
        if "/versions" in self.path:
            data = {"versions": ["v1.12"], "unstable_features": {}}
        elif "/whoami" in self.path or "/register" in self.path:
            data = {"user_id": "@discordbot:example.org", "is_guest": False}
        elif "/config" in self.path:
            data = {"m.upload.size": 26214400}
        elif "/ping" in self.path:
            data = {"duration_ms": 1}
        else:
            data = {}
        body = json.dumps(data).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    do_GET = do_POST = do_PUT = reply


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", default="./mautrix-discord")
    args = parser.parse_args()
    binary = str(pathlib.Path(args.binary).resolve())
    repo = pathlib.Path(__file__).resolve().parents[1]
    with tempfile.TemporaryDirectory(prefix="discord-v2-smoke-") as temporary:
        work = pathlib.Path(temporary)
        hs = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Homeserver)
        threading.Thread(target=hs.serve_forever, daemon=True).start()
        with socket.socket() as sock:
            sock.bind(("127.0.0.1", 0))
            app_port = sock.getsockname()[1]
        config = yaml.safe_load((repo / "example-config.yaml").read_text())
        config["database"].update(type="sqlite3-fk-wal", uri=f"file:{work}/bridge.db?_txlock=immediate")
        config["homeserver"].update(address=f"http://127.0.0.1:{hs.server_port}", domain="example.org")
        config["appservice"].update(address=f"http://127.0.0.1:{app_port}", hostname="127.0.0.1", port=app_port,
                                    as_token="smoke-appservice-token", hs_token="smoke-homeserver-token")
        config["bridge"]["permissions"] = {"*": "relay", "@admin:example.org": "admin"}
        config["provisioning"]["shared_secret"] = "smoke-provisioning-secret"
        config["logging"]["writers"] = [{"type": "stdout", "format": "json", "min_level": "debug"}]
        path = work / "config.yaml"
        path.write_text(yaml.safe_dump(config, sort_keys=False))

        def request(path, auth=True, admin=True):
            uid = "@admin:example.org" if admin else "@guest:example.org"
            req = urllib.request.Request(f"http://127.0.0.1:{app_port}/_matrix/provision/{path}?user_id={uid}")
            if auth:
                req.add_header("Authorization", "Bearer smoke-provisioning-secret")
            try:
                with urllib.request.urlopen(req, timeout=2) as resp:
                    return resp.status, json.load(resp)
            except urllib.error.HTTPError as err:
                return err.code, json.load(err)

        with (work / "bridge.log").open("w") as log:
            proc = subprocess.Popen([binary, "-c", str(path)], cwd=work, stdout=log, stderr=subprocess.STDOUT)
            try:
                for _ in range(100):
                    if proc.poll() is not None:
                        raise RuntimeError(f"Bridge exited {proc.returncode}")
                    try:
                        status, data = request("v3/discord/channels")
                        if status == 200:
                            break
                    except (OSError, ValueError):
                        pass
                    time.sleep(0.1)
                else:
                    raise RuntimeError("Provisioning did not start")
                assert data == {"channels": []}, data
                status, data = request("v3/login/flows")
                assert status == 200 and [flow["id"] for flow in data["flows"]] == ["bot-token"], (status, data)
                for route in ("v3/discord/channels", "v1/ping"):
                    status, data = request(route, auth=False)
                    assert status in (401, 403), (status, data)
                status, data = request("v3/discord/channels", admin=False)
                assert status == 403, (status, data)
                status, data = request("v1/ping")
                assert status == 200 and data["Discord"]["logged_in"] is False, (status, data)
                print("Startup, v3/v1 provisioning, bot-only defaults, and authentication boundaries passed.")
            except Exception:
                log.flush()
                print((work / "bridge.log").read_text()[-4000:])
                raise
            finally:
                proc.terminate()
                try:
                    proc.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait(timeout=5)
                hs.shutdown()
                hs.server_close()


if __name__ == "__main__":
    main()
