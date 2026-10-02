#!/usr/bin/env python3
"""Actual Rust controller + Rust plugin processes, isolated on loopback with disposable state."""
import base64
import hashlib
import hmac
import http.cookiejar
import json
import os
from pathlib import Path
import subprocess
import ssl
import tempfile
import time
import unittest
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
BIN = Path(os.environ.get("STULP_TEST_BIN_DIR", str(ROOT / "target/debug")))
APP = "com.stulp.virtualdevices"

def certificate(directory):
    """Een tijdelijke P-256 identiteit voor stulp.test/localhost/127.0.0.1; alleen voor deze testrun."""
    directory = Path(directory)
    subprocess.run(["openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes",
                    "-keyout", str(directory / "key.pem"), "-out", str(directory / "cert.pem"), "-subj", "/CN=stulp.test",
                    "-addext", "subjectAltName=DNS:stulp.test,DNS:localhost,IP:127.0.0.1", "-days", "1"],
                   check=True, capture_output=True, timeout=30)

class Chain:
    def __init__(self, document, app=APP, binary="stulp-virtualdevices", unix=None, plugin_cwd=None, log_level="info", tls=None):
        self.processes = []
        self.logs = tempfile.TemporaryFile()
        self.scheme = "https" if tls else "http"
        self.ssl = ssl.create_default_context(cafile=str(tls / "cert.pem")) if tls else None
        self.http = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()), urllib.request.HTTPSHandler(context=self.ssl))
        self.server = self.spawn([str(BIN / "stulp-host"), "--document", str(document), "serve", "--listen", "127.0.0.1:0", "--token", "integration-key", "--attach-port", "127.0.0.1:0", "--log-level", log_level] + (["--tls-cert", str(tls / "cert.pem"), "--tls-key", str(tls / "key.pem")] if tls else ["--attach-plaintext"]) + (["--attach", str(unix)] if unix else []))
        self.address = self.server.stdout.readline().decode().strip().split("://", 1)[1]
        attach = self.server.stdout.readline().decode().strip().split("://", 1)[1]
        self.base = self.scheme + "://" + self.address
        self.attach = attach
        if unix:
            assert self.server.stdout.readline().decode().strip().endswith("unix://" + str(unix))
        self.get("/integration-key")
        token = base64.urlsafe_b64encode(hmac.new(b"integration-secret", b"token\0" + app.encode() + b"\0", hashlib.sha256).digest()).decode().rstrip("=")
        env = dict(os.environ, STULP_ATTACH=attach, STULP_ATTACH_TOKEN=token, STULP_ATTACH_PLAINTEXT="1")
        env.pop("STULP_ATTACH_CA", None)
        env.pop("STULP_ATTACH_INSECURE", None)
        if tls:
            env.pop("STULP_ATTACH_PLAINTEXT", None)
            env["STULP_ATTACH_CA"] = str(tls / "cert.pem")
        env.pop("STULP_SOCKET", None)
        if unix:
            env["STULP_ATTACH"] = str(unix)
            env.pop("STULP_ATTACH_TOKEN", None)
            env.pop("STULP_ATTACH_PLAINTEXT", None)
        self.plugin = self.spawn([str(BIN / binary)], env=env, cwd=plugin_cwd) if binary else None

    def spawn(self, args, **kwargs):
        process = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=self.logs, **kwargs)
        self.processes.append(process)
        return process

    def get(self, path):
        with self.http.open(self.base + path, timeout=5) as response:
            return response.read()

    def request(self, method, path, body=None, origin=None):
        headers = {"Content-Type": "application/json"}
        if origin is not None:
            headers["Origin"] = origin
        request = urllib.request.Request(self.base + path,
            data=None if body is None else json.dumps(body).encode(), method=method, headers=headers)
        with self.http.open(request, timeout=10) as response:
            return response.status, json.loads(response.read())

    def mcp(self, name, arguments):
        request = urllib.request.Request(self.base + "/mcp/integration-key", data=json.dumps({"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":name,"arguments":arguments}}).encode(), headers={"Content-Type":"application/json","Accept":"application/json, text/event-stream"})
        with self.http.open(request, timeout=12) as response:
            value=json.loads(response.read())
        assert "error" not in value, value
        assert not value["result"].get("isError"), value
        return value["result"]["structuredContent"]

    def device(self):
        return json.loads(self.get("/api/manager/devices/device/switch"))

    def wait_value(self, expected):
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            device = self.device()
            # device.init publishes its initial values before its final acknowledgement.
            # A controller may invoke callbacks only after startup has completed.
            app = json.loads(self.get("/api/manager/apps/app/" + APP))
            if app["state"] == "running" and device["available"] and device["capabilitiesObj"]["onoff"]["value"] is expected:
                return device
            time.sleep(.02)
        self.logs.seek(0)
        raise AssertionError("Plugin did not restore value: " + self.logs.read().decode())

    def set(self, value):
        request = urllib.request.Request(self.base + "/api/manager/devices/device/switch/capability/onoff", data=json.dumps({"value": value}).encode(), method="PUT", headers={"Content-Type": "application/json"})
        try:
            with self.http.open(request, timeout=5) as response:
                assert response.status == 200
        except urllib.error.HTTPError as error:
            detail = error.read().decode()
            error.close()
            self.logs.seek(0)
            raise AssertionError(f"Capability failed: {error.code} {detail}; logs: {self.logs.read().decode()}") from error

    def close(self):
        for process in reversed(self.processes):
            process.terminate()
        for process in reversed(self.processes):
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
            process.stdout.close()
        self.logs.close()

class Plugins(unittest.TestCase):
    def test_https_and_encrypted_attach_control_persist_and_reject_plaintext(self):
        import socket
        with tempfile.TemporaryDirectory(prefix="stulp-tls-chain-") as temporary:
            directory=Path(temporary)
            certificate(directory)
            path=directory / "test.json"
            path.write_text(json.dumps({"version":2,"apps":[{"id":APP,"enabled":True}],"system":{"attachSecret":"integration-secret"},"devices":[{"id":"switch","name":"TLS switch","appId":APP,"driverId":"switch","data":{"id":"tls"},"capabilities":["onoff"],"store":{"onoff":False}}]}))
            chain=Chain(path,tls=directory)
            try:
                chain.wait_value(False)
                with chain.http.open(chain.base + "/api/stulp/events?manager=devices", timeout=5) as events:
                    self.assertEqual(events.readline(), b": connected\n")
                    self.assertEqual(events.readline(), b"\n")
                    chain.set(True)
                    chain.wait_value(True)
                    # Keep the HTTPS event stream open while a separate request mutates state.
                    for _ in range(20):
                        line=events.readline()
                        if line.startswith(b"data:"):
                            self.assertEqual(json.loads(line[5:])["manager"],"devices")
                            break
                    else: self.fail("no device event on HTTPS subscription")
                # A stalled TLS peer must not hold the single controller owner.
                host,port=chain.attach.rsplit(":",1)
                with socket.create_connection((host,int(port)),timeout=3) as stalled:
                    chain.set(False)
                    chain.wait_value(False)
                    # Plain app greetings are not disclosed before TLS is established.
                    stalled.settimeout(.1)
                    with self.assertRaises(TimeoutError): stalled.recv(1)
                chain.set(True)
                chain.wait_value(True)
            finally: chain.close()
            # A cold application restart keeps the state and reconnects through verified TLS.
            chain=Chain(path,tls=directory)
            try: chain.wait_value(True)
            finally: chain.close()

    def test_owned_plugin_log_level_and_last_line_survive_process_exit(self):
        import sys
        with tempfile.TemporaryDirectory(prefix="stulp-plugin-logs-", dir="/tmp") as temporary:
            root = Path(temporary)
            bundle = root / "bundle"
            bundle.mkdir()
            (bundle / "app.json").write_bytes((ROOT / "plugins/virtualdevices/app.json").read_bytes())
            launcher = bundle / APP
            launcher.write_text("#!" + sys.executable + "\nimport sys\nprint('debug\\thidden fixture')\nprint('info\\tinfo fixture')\nprint('warn\\twarning fixture')\nsys.stderr.write('error\\tlast fixture words')\nsys.stderr.flush()\n")
            launcher.chmod(0o700)
            doc = root / "state.json"
            doc.write_text(json.dumps({"version":2,"apps":[{"id":APP,"root":str(bundle),"enabled":True}]}))
            chain = Chain(doc,binary=None,log_level="warn")
            try:
                deadline = time.monotonic() + 5
                while time.monotonic() < deadline:
                    logs = os.pread(chain.logs.fileno(),65536,0).decode()
                    if 'last fixture words' in logs:
                        break
                    time.sleep(.05)
                self.assertIn('level=ERROR app=' + APP + ' msg="last fixture words"',logs)
                self.assertIn('level=WARN app=' + APP + ' msg="warning fixture"',logs)
                self.assertNotIn('hidden fixture',logs)
                self.assertNotIn('info fixture',logs)
            finally:
                chain.close()

    def test_stopped_local_bundle_ui_and_locale_without_embedded_manifest(self):
        with tempfile.TemporaryDirectory(prefix="stulp-local-ui-", dir="/tmp") as temporary:
            root = Path(temporary)
            bundle = root / "bundle"
            (bundle / "settings").mkdir(parents=True)
            (bundle / "locales").mkdir()
            (bundle / "app.json").write_text(json.dumps({"id":APP,"version":"1.0","sdk":3,"name":{"en":"Fixture"}}))
            (bundle / "settings/index.html").write_text("<html><head></head><body>Offline settings</body></html>")
            (bundle / "locales/nl.json").write_text("invalid")
            (bundle / "locales/en.json").write_text(json.dumps({"title":"English fallback"}))
            (root / "outside.txt").write_text("private-outside")
            (bundle / "settings/escape.txt").symlink_to(root / "outside.txt")
            doc = root / "state.json"
            doc.write_text(json.dumps({"version":2,"apps":[{"id":APP,"root":str(bundle),"enabled":False}]}))
            chain = Chain(doc,binary=None)
            try:
                page = chain.get("/app-ui/" + APP + "/settings/").decode()
                self.assertIn("Offline settings",page)
                self.assertIn("English fallback",page)
                self.assertIn("window.__STULP_CONTEXT__",page)
                # A filesystem lookup failure is also an optional locale failure.
                # A self-referencing link gives ELOOP even when tests run as root.
                (bundle / "locales/nl.json").unlink()
                (bundle / "locales/nl.json").symlink_to("nl.json")
                page = chain.get("/app-ui/" + APP + "/settings/").decode()
                self.assertIn("Offline settings", page)
                self.assertIn("English fallback", page)
                app = json.loads(chain.get("/api/manager/apps/app/" + APP))
                self.assertTrue(app["settings"])
                with self.assertRaises(urllib.error.HTTPError) as denied:
                    chain.get("/app-ui/" + APP + "/settings/escape.txt")
                self.assertEqual(denied.exception.code,404)
                denied.exception.close()
            finally:
                chain.close()

    def test_explicit_unix_attach_authenticates_controls_and_cleans_socket(self):
        import stat
        with tempfile.TemporaryDirectory(prefix="stulp-unix-", dir="/tmp") as temporary:
            path = Path(temporary) / "test.json"
            socket = Path(temporary) / "app.sock"
            path.write_text(json.dumps({"version":2,"apps":[{"id":APP,"enabled":True}],"devices":[{"id":"switch","appId":APP,"driverId":"switch","capabilities":["onoff"],"data":{"id":"virtual-test"},"store":{"onoff":False}}],"system":{"attachSecret":"integration-secret"}}))
            chain = Chain(path, unix=socket)
            try:
                self.assertEqual(stat.S_IMODE(socket.stat().st_mode), 0o600)
                chain.wait_value(False)
                chain.set(True)
                chain.wait_value(True)
            finally:
                chain.close()
            self.assertFalse(socket.exists())

    def test_owned_process_crash_restart_disable_enable_and_uninstall(self):
        import signal
        import sys
        with tempfile.TemporaryDirectory(prefix="stulp-owned-chain-") as temporary:
            path = Path(temporary) / "test.json"
            bundle = Path(temporary) / "bundle"
            bundle.mkdir()
            (bundle / "app.json").write_bytes((ROOT / "plugins/virtualdevices/app.json").read_bytes())
            pids = bundle / "pids"
            launcher = bundle / APP
            launcher.write_text("#!" + sys.executable + "\nimport os\nwith open(" + repr(str(pids)) + ", 'a') as f: f.write(str(os.getpid()) + '\\n')\nos.execv(" + repr(str(BIN / "stulp-virtualdevices")) + ", ['stulp-virtualdevices'])\n")
            launcher.chmod(0o700)
            path.write_text(json.dumps({"version":2,"apps":[{"id":"test.missing","enabled":True,"root":str(bundle / "missing")},{"id":APP,"enabled":True,"root":str(bundle)}],"system":{"attachSecret":"integration-secret"},"devices":[{"id":"switch","name":"Owned","appId":APP,"driverId":"switch","data":{"id":"owned"},"capabilities":["onoff"],"store":{"value":False}}]}))
            chain = Chain(path, binary=None)
            def generation(count):
                deadline = time.monotonic() + 15
                while time.monotonic() < deadline:
                    values = pids.read_text().splitlines() if pids.exists() else []
                    if len(values) >= count:
                        chain.wait_value(count > 1)
                        return int(values[-1])
                    time.sleep(.05)
                chain.logs.seek(0)
                self.fail("Owned app did not start: " + chain.logs.read().decode())
            try:
                first = generation(1)
                chain.set(True)
                os.kill(first, signal.SIGKILL)
                second = generation(2)
                self.assertNotEqual(first, second)
                self.assertEqual(chain.request("POST", "/api/manager/apps/app/" + APP + "/restart"), (200, True))
                third = generation(3)
                self.assertNotEqual(second, third)
                self.assertEqual(chain.request("PUT", "/api/manager/apps/app/" + APP + "/disable"), (200, True))
                deadline = time.monotonic() + 10
                while json.loads(chain.get("/api/manager/apps/app/" + APP))["state"] != "stopped":
                    self.assertLess(time.monotonic(), deadline)
                    time.sleep(.02)
                chain.request("PUT", "/api/manager/apps/app/" + APP + "/enable")
                generation(4)
                _, removed = chain.request("DELETE", "/api/manager/apps/app/" + APP)
                self.assertEqual(removed["devices"], 1)
                saved = json.loads(path.read_text())
                self.assertEqual(saved["devices"], [])
                self.assertEqual([a["id"] for a in saved["apps"]], ["test.missing"])
                self.assertEqual(json.loads(chain.get("/api/manager/apps/app/test.missing"))["state"], "crashed")
                self.assertTrue(bundle.exists())
            finally:
                chain.close()
                # Alleen door deze test gestarte processen, ook bij een falende assertion.
                for pid in pids.read_text().splitlines() if pids.exists() else []:
                    try: os.kill(int(pid), signal.SIGTERM)
                    except ProcessLookupError: pass

    def test_backup_restore_streams_bundles_restarts_plugins_and_rejects_bad_archives(self):
        self.backup_restore(False)

    def test_https_backup_restore_streams_large_bundles_and_restarts_plugins(self):
        self.backup_restore(True)

    def backup_restore(self, tls):
        import io
        import zipfile
        import sys
        import signal
        import http.client
        with tempfile.TemporaryDirectory(prefix="stulp-backup-chain-") as temporary:
            directory=Path(temporary)
            path=directory / "test.json"
            bundle=directory / "bundle"
            bundle.mkdir()
            (bundle / "app.json").write_bytes((ROOT / "plugins/virtualdevices/app.json").read_bytes())
            payload=os.urandom(2 << 20)
            (bundle / "payload.bin").write_bytes(payload)
            pids=directory / "pids"
            launcher=bundle / APP
            launcher.write_text("#!" + sys.executable + "\nimport os\nwith open(" + repr(str(pids)) + ", 'a') as f: f.write(str(os.getpid()) + '\\n')\nos.execv(" + repr(str(BIN / "stulp-virtualdevices")) + ", ['stulp-virtualdevices'])\n")
            launcher.chmod(0o700)
            path.write_text(json.dumps({"version":2,"apps":[{"id":APP,"enabled":True,"root":str(bundle)}],"system":{"attachSecret":"integration-secret"},"devices":[{"id":"switch","name":"Before","appId":APP,"driverId":"switch","data":{"id":"backup"},"capabilities":["onoff"],"store":{"onoff":False}}]}))
            if tls:
                certificate(directory)
            chain=Chain(path,binary=None,tls=directory if tls else None)
            try:
                chain.wait_value(False)
                original_pid=pids.read_text().splitlines()[-1]
                archive=chain.get("/api/stulp/backup")
                self.assertGreater(len(archive),1 << 20)
                with zipfile.ZipFile(io.BytesIO(archive)) as z:
                    self.assertEqual(z.read("apps/000/payload.bin"),payload)
                    self.assertEqual(json.loads(z.read("backup.json"))["format"],1)
                    self.assertIsNone(z.testzip())
                chain.set(True)
                current=path.read_bytes()
                req=urllib.request.Request(chain.base+"/api/stulp/restore",data=b"broken",headers={"Content-Type":"application/zip"})
                with self.assertRaises(urllib.error.HTTPError) as error: chain.http.open(req,timeout=10)
                self.assertEqual(error.exception.code,422)
                error.exception.close()
                self.assertEqual(path.read_bytes(),current)
                self.assertEqual(pids.read_text().splitlines()[-1],original_pid)
                # Een ZIP groter dan Lean's gewone 1 MiB bodylimiet, met Content-Length.
                req=urllib.request.Request(chain.base+"/api/stulp/restore",data=archive,headers={"Content-Type":"application/zip"})
                with chain.http.open(req,timeout=20) as response: restored=json.loads(response.read())
                self.assertTrue(restored["restored"])
                self.assertTrue(Path(restored["previousDocument"]).is_file())
                chain.wait_value(False)
                self.assertNotEqual(pids.read_text().splitlines()[-1],original_pid)
                saved=json.loads(path.read_text())
                new_root=Path(saved["apps"][0]["root"])
                self.assertEqual((new_root / "payload.bin").read_bytes(),payload)
                self.assertEqual(new_root.parent,Path(str(path)+".apps").resolve())
                chain.set(True)
                # Dezelfde grote backup met HTTP chunked framing.
                cookie=urllib.request.Request(chain.base+"/")
                for handler in chain.http.handlers:
                    if isinstance(handler,urllib.request.HTTPCookieProcessor): handler.cookiejar.add_cookie_header(cookie)
                connection=http.client.HTTPSConnection(chain.address,context=chain.ssl,timeout=30) if tls else http.client.HTTPConnection(chain.address,timeout=20)
                try:
                    connection.request("POST","/api/stulp/restore",body=(archive[i:i+8192] for i in range(0,len(archive),8192)),headers={"Content-Type":"application/zip","Cookie":cookie.get_header("Cookie")},encode_chunked=True)
                    response=connection.getresponse()
                    self.assertEqual(response.status,200,response.read() if response.status!=200 else None)
                    self.assertTrue(json.loads(response.read())["restored"])
                finally: connection.close()
                chain.wait_value(False)
                # Een alles-in-één backup van een HopOS-installatie bevat externe apps
                # zonder bundels. De bestaande native Rust-installatie moet blijven werken.
                current=json.loads(path.read_text())
                installed_root=current["apps"][0]["root"]
                current["apps"][0]["root"]=""
                current["devices"][0]["name"]="Imported configuration"
                data=io.BytesIO()
                with zipfile.ZipFile(data,"w",compression=zipfile.ZIP_DEFLATED) as z:
                    z.writestr("backup.json",json.dumps({"format":1,"apps":[{"id":APP}]}))
                    z.writestr("stulp.json",json.dumps(current))
                chain.set(True)
                req=urllib.request.Request(chain.base+"/api/stulp/restore",data=data.getvalue(),headers={"Content-Type":"application/zip"})
                with chain.http.open(req,timeout=20) as response:
                    self.assertTrue(json.loads(response.read())["restored"])
                device=chain.wait_value(False)
                self.assertEqual(device["name"],"Imported configuration")
                self.assertEqual(json.loads(path.read_text())["apps"][0]["root"],installed_root)
                chain.set(True)
                chain.wait_value(True)
            finally:
                chain.close()
                for pid in pids.read_text().splitlines() if pids.exists() else []:
                    try: os.kill(int(pid),signal.SIGTERM)
                    except ProcessLookupError: pass

    def test_cli_management_and_plugin_commands(self):
        with tempfile.TemporaryDirectory(prefix="stulp-cli-") as temporary:
            directory=Path(temporary)
            path=directory / "test.json"
            bundle=directory / "bundle"
            bundle.mkdir()
            (bundle / "app.json").write_bytes((ROOT / "plugins/virtualdevices/app.json").read_bytes())
            launcher=bundle / APP
            launcher.write_text('#!/bin/sh\nexec "'+str(BIN / "stulp-virtualdevices")+'"\n')
            launcher.chmod(0o700)
            def run(*args):
                result=subprocess.run([str(BIN / "stulp-host"),"--document",str(path),*args],stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=15)
                self.assertEqual(result.returncode,0,(args,result.stderr.decode()))
                return result.stdout.decode()
            run("install",str(bundle))
            token=run("attach-token",APP)
            self.assertEqual(token,run("attach-token",APP))
            run("attach-token","--rotate")
            self.assertNotEqual(token,run("attach-token",APP))
            device=json.loads(run("add-device","--name","CLI switch","--data",'{"id":"cli"}',APP,"switch"))
            self.assertEqual(device["name"],"CLI switch")
            self.assertEqual(len(json.loads(run("devices",APP))),1)
            self.assertTrue(json.loads(run("inspect",APP)))
            run("run","--once",APP)
            value=json.loads(run("invoke",APP,device["id"],"onoff","true"))
            self.assertIs(value["state"]["onoff"],True)
            disabled=json.loads(path.read_text())
            disabled["apps"][0]["enabled"]=False
            path.write_text(json.dumps(disabled))
            run("run","--once",APP)
            self.assertFalse(json.loads(path.read_text())["apps"][0]["enabled"])
            process=subprocess.Popen([str(BIN / "stulp-host"),"--document",str(path),"run",APP],stdout=subprocess.PIPE,stderr=subprocess.PIPE)
            try:
                time.sleep(.4)
                process.terminate()
                process.wait(timeout=10)
                self.assertEqual(process.returncode,0,process.stderr.read().decode())
            finally:
                if process.poll() is None: process.kill(); process.wait()
                process.stdout.close(); process.stderr.close()
            run("uninstall",APP)
            self.assertEqual(json.loads(run("apps")),[])

    def test_announced_plugin_requires_installation_before_receiving_state(self):
        with tempfile.TemporaryDirectory(prefix="stulp-offered-chain-") as temporary:
            path = Path(temporary) / "test.json"
            path.write_text(json.dumps({"version":2,"system":{"attachSecret":"integration-secret"}}))
            chain = Chain(path)
            try:
                deadline = time.monotonic() + 10
                while True:
                    apps = json.loads(chain.get("/api/manager/apps/app"))
                    if APP in apps: break
                    self.assertLess(time.monotonic(), deadline)
                    time.sleep(.02)
                self.assertTrue(apps[APP]["offered"])
                self.assertFalse(apps[APP]["enabled"])
                self.assertEqual(apps[APP]["state"], "stopped")
                before = path.read_bytes()
                time.sleep(1.25)
                self.assertEqual(path.read_bytes(), before)
                _, app = chain.request("POST", "/api/stulp/apps/" + APP + "/install")
                self.assertTrue(app["enabled"])
                self.assertFalse(app["offered"])
                while json.loads(chain.get("/api/manager/apps/app/" + APP))["state"] != "running":
                    self.assertLess(time.monotonic(), deadline)
                    time.sleep(.02)
                _, token = chain.request("GET", "/api/stulp/attach-token/" + APP)
                self.assertTrue(token["known"])
                self.assertTrue(token["token"])
            finally:
                chain.close()

    def test_browser_pairing_creates_initializes_and_deduplicates(self):
        with tempfile.TemporaryDirectory(prefix="stulp-pair-chain-") as temporary:
            path = Path(temporary) / "test.json"
            path.write_text(json.dumps({"version": 2, "apps": [{"id": APP, "enabled": True}], "system": {"attachSecret": "integration-secret"}}))
            chain = Chain(path)
            try:
                deadline = time.monotonic() + 10
                while json.loads(chain.get("/api/manager/apps/app/" + APP))["state"] != "running":
                    self.assertLess(time.monotonic(), deadline)
                    time.sleep(.02)
                drivers = json.loads(chain.get("/api/manager/drivers/driver"))
                self.assertEqual(drivers["stulp:app:" + APP + ":switch"]["customPairViews"], ["name"])
                with self.assertRaises(urllib.error.HTTPError) as denied:
                    chain.request("POST", "/api/stulp/pair", {"appId": APP, "driverId": "switch"}, origin="https://unrelated.invalid")
                self.assertEqual(denied.exception.code, 403)
                denied.exception.close()
                status, session = chain.request("POST", "/api/stulp/pair", {"appId": APP, "driverId": "switch"})
                self.assertEqual(status, 201)
                self.assertIn("create", session["handlers"])
                route = "/api/stulp/pair/" + session["id"]
                _, candidate = chain.request("POST", route + "/emit/create", {"name": "Via browser"})
                _, devices = chain.request("POST", route + "/emit/list_devices", {})
                self.assertEqual(devices, [candidate])
                adopt = "/api/stulp/apps/" + APP + "/drivers/switch/pair/devices"
                status, device = chain.request("POST", adopt, candidate)
                self.assertEqual(status, 201)
                self.assertTrue(device["available"])
                self.assertFalse(device["capabilitiesObj"]["onoff"]["value"])
                self.assertNotIn("store", device)
                _, again = chain.request("POST", adopt, candidate)
                self.assertEqual(again["id"], device["id"])
                self.assertEqual(len(json.loads(path.read_text())["devices"]), 1)
                chain.request("PUT", "/api/manager/devices/device/" + device["id"] + "/capability/onoff", {"value": True})
                _, current = chain.request("GET", "/api/manager/devices/device/" + device["id"])
                self.assertTrue(current["capabilitiesObj"]["onoff"]["value"])
                settings_route = "/api/manager/devices/device/" + device["id"] + "/settings"
                _, configured = chain.request("PUT", settings_route, {"iconOverride": "light"})
                self.assertEqual(configured["settings"]["iconOverride"], "light")
                saved = json.loads(path.read_text())["devices"][0]
                self.assertEqual(saved["settings"]["iconOverride"], "light")
                self.assertEqual(chain.request("DELETE", route), (200, True))
                self.assertEqual(chain.request("DELETE", route), (200, True))
                with self.assertRaises(urllib.error.HTTPError) as gone:
                    chain.request("POST", route + "/emit/list_devices", {})
                self.assertEqual(gone.exception.code, 404)
                gone.exception.close()
            finally:
                chain.close()

    def test_scene_apply_restore_manual_override_and_flow_use_real_plugin(self):
        with tempfile.TemporaryDirectory(prefix="stulp-scene-chain-") as temporary:
            path = Path(temporary) / "test.json"
            path.write_text(json.dumps({"version":2,"apps":[{"id":APP,"enabled":True}],"system":{"attachSecret":"integration-secret"},"devices":[{"id":"switch","name":"Lamp","appId":APP,"driverId":"switch","data":{"id":"scene-test"},"capabilities":["onoff"]}]}))
            chain = Chain(path)
            try:
                chain.wait_value(False)
                _, scene = chain.request("POST", "/api/stulp/scenes", {"name":"Movie","states":[{"deviceId":"switch","capabilityId":"onoff","value":True}]})
                route = "/api/manager/devices/device/scene:" + scene["id"] + "/capability/onoff"
                self.assertEqual(chain.request("PUT", route, {"value":True}), (200,True))
                chain.wait_value(True)
                stored = json.loads(path.read_text())["scenes"][0]
                self.assertTrue(stored["active"])
                self.assertFalse(stored["previous"][0]["value"])
                self.assertNotIn("previous", json.loads(chain.get("/api/stulp/scenes/" + scene["id"])))
                self.assertEqual(chain.request("PUT", route, {"value":False}), (200,True))
                chain.wait_value(False)
                self.assertFalse(json.loads(path.read_text())["scenes"][0]["active"])
                chain.request("PUT", route, {"value":True})
                chain.set(False)
                deadline = time.monotonic() + 5
                while json.loads(path.read_text())["scenes"][0]["active"]:
                    self.assertLess(time.monotonic(),deadline)
                    time.sleep(.02)
                _, flow = chain.request("POST", "/api/manager/flow/flow", {"name":"Scene flow","enabled":True,"nodes":[{"id":"start","step":{"appId":"stulp","cardType":"trigger","cardId":"manual"}},{"id":"set","step":{"appId":"stulp","cardType":"action","cardId":"capability.onoff.turn_on","args":{"device":{"$device":"scene:"+scene["id"]}}}}],"edges":[{"id":"edge","from":"start","to":"set"}]})
                status, _ = chain.request("POST", "/api/stulp/flows/" + flow["id"] + "/run")
                self.assertEqual(status,200)
                chain.wait_value(True)
                self.assertTrue(json.loads(path.read_text())["scenes"][0]["active"])
            finally:
                chain.close()

    def test_mcp_creates_controls_scenes_and_edits_runs_flows(self):
        with tempfile.TemporaryDirectory(prefix="stulp-mcp-") as temporary:
            path = Path(temporary) / "test.json"
            path.write_text(json.dumps({"version":2,"apps":[{"id":APP,"enabled":True}],"system":{"attachSecret":"integration-secret"}}))
            chain=Chain(path)
            try:
                deadline=time.monotonic()+10
                while json.loads(chain.get("/api/manager/apps/app/"+APP))["state"]!="running":
                    if time.monotonic()>deadline: self.fail("plugin did not start")
                    time.sleep(.02)
                context=chain.mcp("system_context",{})["context"]
                self.assertEqual(context["timezone"],"UTC")
                created=chain.mcp("devices_create",{"type":"virtual_switch","name":"MCP switch"})
                device=created["device"]["id"]
                self.assertFalse(created["device"]["capabilities"]["onoff"]["value"])
                written=chain.mcp("devices_write",{"deviceId":device,"capabilityId":"onoff","value":True})
                self.assertTrue(written["accepted"])
                detail=chain.mcp("devices_list",{"deviceId":device})["devices"][0]
                self.assertTrue(detail["capabilities"]["onoff"]["value"])
                cards=chain.mcp("flow_cards_list",{"deviceId":device,"kind":"action"})["cards"]
                self.assertIn("capability.onoff.turn_off",[c["id"] for c in cards])
                chain.mcp("flow_action_run",{"appId":"stulp","cardId":"capability.onoff.turn_off","args":{"device":device}})
                self.assertFalse(chain.mcp("devices_list",{"deviceId":device})["devices"][0]["capabilities"]["onoff"]["value"])
                flow=chain.mcp("flows_create",{"name":"MCP flow"})["flow"]["id"]
                chain.mcp("flows_add_cards",{"flowId":flow,"nodes":[{"id":"t","x":0,"y":0,"step":{"appId":"stulp","cardType":"trigger","cardId":"time_at","args":{"time":"23:59"}}},{"id":"a","x":100,"y":0,"step":{"appId":"stulp","cardType":"action","cardId":"capability.onoff.turn_on","args":{"device":device}}}]})
                chain.mcp("flows_connect_cards",{"flowId":flow,"fromNodeId":"t","toNodeId":"a","edgeId":"edge"})
                chain.mcp("flows_configure_card",{"flowId":flow,"nodeId":"a","x":100,"y":200})
                execution=chain.mcp("flows_run",{"flowId":flow})["execution"]
                self.assertTrue(execution["success"])
                self.assertTrue(chain.mcp("devices_list",{"deviceId":device})["devices"][0]["capabilities"]["onoff"]["value"])
                chain.mcp("flows_update",{"flowId":flow,"name":"Renamed","enabled":False})
                chain.mcp("flows_disconnect_cards",{"flowId":flow,"edgeId":"edge"})
                chain.mcp("flows_remove_card",{"flowId":flow,"nodeId":"a"})
                read=chain.mcp("flows_list",{"flowId":flow})["flows"][0]
                self.assertEqual(read["nodeCount"],1)
                chain.mcp("flows_delete",{"flowId":flow})
                _, scene=chain.request("POST","/api/stulp/scenes",{"name":"MCP Scene","kind":"switch","states":[{"deviceId":device,"capabilityId":"onoff","value":False}]})
                on=chain.mcp("devices_write",{"deviceId":"scene:"+scene["id"],"capabilityId":"onoff","value":True})
                self.assertTrue(on["sceneActivation"]["success"])
                off=chain.mcp("devices_write",{"deviceId":"scene:"+scene["id"],"capabilityId":"onoff","value":False})
                self.assertTrue(off["sceneActivation"]["success"])
                self.assertNotIn("store", json.dumps(chain.mcp("devices_list",{"deviceId":device})))
            finally:
                chain.close()

    def test_settings_asset_and_api_use_actual_somfy_plugin(self):
        app = "com.stulp.somfy"
        with tempfile.TemporaryDirectory(prefix="stulp-plugin-ui-") as temporary:
            path = Path(temporary) / "test.json"
            path.write_text(json.dumps({"version": 2, "apps": [{"id": app, "enabled": True}], "system": {"attachSecret": "integration-secret"}}))
            chain = Chain(path, app, "stulp-somfy")
            try:
                deadline = time.monotonic() + 10
                while True:
                    info = json.loads(chain.get("/api/manager/apps/app/" + app))
                    if info["state"] == "running":
                        break
                    if time.monotonic() >= deadline:
                        self.fail("Somfy plugin did not become ready")
                    time.sleep(.02)
                self.assertTrue(info["settings"])
                cards = json.loads(chain.get("/api/stulp/flow/cards"))
                scenario = next(c for c in cards["actions"] if c["appId"] == app and c["id"] == "activate_scenario")
                self.assertTrue(scenario["available"])
                self.assertIn("scenario", scenario["registration"]["autocomplete"])
                self.assertEqual(scenario["scope"], "app")
                with self.assertRaises(urllib.error.HTTPError) as invalid_card:
                    chain.request("POST", "/api/stulp/flow/autocomplete", {"appId":app,"cardType":"action","cardId":"unknown","argument":"scenario"})
                self.assertEqual(invalid_card.exception.code, 502)
                self.assertIn("unknown autocomplete", invalid_card.exception.read().decode())
                invalid_card.exception.close()
                page = chain.get("/app-ui/" + app + "/settings/").decode()
                self.assertIn('window.__STULP_CONTEXT__=', page)
                self.assertIn('"appId":"com.stulp.somfy"', page)
                self.assertIn('/assets/app-frame.css', page)
                self.assertIn('Stulp', chain.get("/app-ui/" + app + "/settings/page.js").decode())
                request = urllib.request.Request(chain.base + "/api/stulp/apps/" + app + "/api/status", data=b"{}", headers={"Content-Type": "application/json"})
                with chain.http.open(request, timeout=5) as response:
                    status = json.loads(response.read())
                self.assertFalse(status["hasPassword"])
                self.assertFalse(status["connected"])
                self.assertNotIn("password", status)
                # The proxy must never turn an unauthenticated 404 into app content.
                with self.assertRaises(urllib.error.HTTPError) as denied:
                    urllib.request.urlopen(chain.base + "/app-ui/" + app + "/settings/", timeout=5)
                self.assertEqual(denied.exception.code, 404)
                denied.exception.close()
                with self.assertRaises(urllib.error.HTTPError) as invalid:
                    chain.get("/app-ui/" + app + "/settings/../app.json")
                invalid.exception.close()
            finally:
                chain.close()


    def test_automatic_flow_uses_real_plugin_observation_and_tokens(self):
        with tempfile.TemporaryDirectory(prefix="stulp-flow-chain-") as temporary:
            path = Path(temporary) / "test.json"
            definition = {"id": "automatic", "name": "Switch automation", "enabled": True,
                "nodes": [{"id": "t", "step": {"appId": "stulp", "cardType": "trigger", "cardId": "capability.onoff.on", "args": {"device": {"$device": "switch"}}}},
                          {"id": "a", "step": {"appId": "stulp", "cardType": "action", "cardId": "notification", "args": {"excerpt": "{{device}} is {{value}}"}}}],
                "edges": [{"from": "t", "to": "a"}]}
            path.write_text(json.dumps({"version": 2, "apps": [{"id": APP, "enabled": True}],
                "devices": [{"id": "switch", "appId": APP, "driverId": "switch", "name": "Test switch", "capabilities": ["onoff"], "data": {"id": "flow-test"}, "store": {"onoff": False}}],
                "flows": [definition], "system": {"attachSecret": "integration-secret"}}))
            chain = Chain(path)
            try:
                chain.wait_value(False)
                chain.set(True)
                deadline = time.monotonic() + 5
                while time.monotonic() < deadline:
                    saved = json.loads(path.read_text())
                    if saved.get("notifications") and saved["flows"][0].get("lastRunAt"):
                        break
                    time.sleep(.02)
                self.assertEqual([n["excerpt"] for n in saved.get("notifications", [])], ["Test switch is true"])
                self.assertFalse(saved["flows"][0].get("lastError"))
                chain.set(True)
                chain.wait_value(True)
                time.sleep(.2)
                self.assertEqual(len(json.loads(path.read_text())["notifications"]), 1)
            finally:
                chain.close()

    def test_virtual_switch_end_to_end_and_restart(self):
        with tempfile.TemporaryDirectory(prefix="stulp-plugin-chain-") as temporary:
            path = Path(temporary) / "test.json"
            path.write_text(json.dumps({"version": 2, "apps": [{"id": APP, "enabled": True}], "devices": [{"id": "switch", "appId": APP, "driverId": "switch", "name": "Test switch", "capabilities": ["onoff"], "data": {"id": "virtual-test"}, "store": {"onoff": False}}], "system": {"attachSecret": "integration-secret"}}))
            chain = Chain(path)
            try:
                chain.wait_value(False)
                chain.set(True)
                chain.wait_value(True)
                # Pass at least one heartbeat while keeping the app's state alive.
                time.sleep(5.2)
                chain.wait_value(True)
            finally:
                chain.close()
            saved = json.loads(path.read_text())
            self.assertIs(saved["devices"][0]["store"]["onoff"], True)
            self.assertNotIn("state", saved["devices"][0])
            chain = Chain(path)
            try:
                chain.wait_value(True)
                chain.set(False)
                chain.wait_value(False)
            finally:
                chain.close()

if __name__ == "__main__":
    unittest.main()
