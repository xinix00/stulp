#!/usr/bin/env python3
"""Real Rust controller + UniFi process + synthetic loopback TLS Protect/RTSP camera."""
import http.server
import json
import os
from pathlib import Path
import re
import shutil
import socketserver
import ssl
import struct
import subprocess
import tempfile
import threading
import time
import unittest
from plugins import Chain, ROOT, certificate

class TCP(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

class HTTP(http.server.ThreadingHTTPServer):
    daemon_threads = True
    def handle_error(self, request, client_address):
        pass  # Expected when the bounded Rust subscription socket closes a non-WebSocket fixture.

@unittest.skipUnless(shutil.which("ffmpeg") and shutil.which("ffprobe") and shutil.which("openssl"), "ffmpeg, ffprobe and openssl required")
class Media(unittest.TestCase):
    def test_actual_unifi_video_through_authenticated_controller(self):
        self.camera(False)

    def test_actual_unifi_video_and_snapshot_through_https_controller(self):
        self.camera(True)

    def camera(self, controller_tls):
        with tempfile.TemporaryDirectory(prefix="stulp-camera-chain-") as directory:
            folder = Path(directory)
            subprocess.run(["ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=2", "-pix_fmt", "yuv420p", "-c:v", "libx264", "-profile:v", "main", "-level", "3.1", "-g", "25", "-bf", "0", "-f", "h264", str(folder / "source.h264")], check=True, capture_output=True)
            subprocess.run(["ffmpeg", "-v", "error", "-i", str(folder / "source.h264"), "-frames:v", "1", str(folder / "snapshot.jpg")], check=True, capture_output=True)
            jpeg = (folder / "snapshot.jpg").read_bytes()
            nalus = [v for v in re.split(b"\x00\x00(?:\x00)?\x01", (folder / "source.h264").read_bytes()) if v]
            sps = next(v for v in nalus if v[0] & 31 == 7)
            pps = next(v for v in nalus if v[0] & 31 == 8)
            frames = [v for v in nalus if v[0] & 31 in (1, 5)]
            import base64
            sdp = "v=0\r\nm=video 0 RTP/AVP 96\r\na=control:track0\r\na=rtpmap:96 H264/90000\r\na=fmtp:96 packetization-mode=1;sprop-parameter-sets=" + base64.b64encode(sps).decode() + "," + base64.b64encode(pps).decode() + "\r\n"
            stop = threading.Event()
            methods = []
            failures = []
            class Camera(socketserver.StreamRequestHandler):
                def handle(self):
                    try:
                        for expected in ("DESCRIBE", "SETUP", "PLAY"):
                            method, _, _ = self.rfile.readline().decode().split()
                            methods.append(method)
                            if method != expected:
                                raise AssertionError((method, expected))
                            headers = {}
                            while True:
                                line = self.rfile.readline().decode().strip()
                                if not line:
                                    break
                                key, value = line.split(":", 1)
                                headers[key.lower()] = value.strip()
                            body = sdp.encode() if method == "DESCRIBE" else b""
                            reply = f"RTSP/1.0 200 OK\r\nCSeq: {headers['cseq']}\r\nSession: synthetic\r\nContent-Length: {len(body)}\r\n\r\n".encode() + body
                            # Deliberately split the response across TCP writes.
                            self.wfile.write(reply[:9]); self.wfile.flush()
                            self.wfile.write(reply[9:]); self.wfile.flush()
                        sequence = 65520
                        index = 0
                        while not stop.is_set():
                            frame = frames[index % len(frames)]
                            timestamp = (0xffffd000 + index * 3600) & 0xffffffff
                            chunks = [frame] if len(frame) <= 1100 else [frame[1:][i:i+1098] for i in range(0, len(frame)-1, 1098)]
                            for i, chunk in enumerate(chunks):
                                marker = i == len(chunks)-1
                                payload = chunk if len(frame) <= 1100 else bytes([(frame[0] & 0xe0) | 28, (frame[0] & 31) | (128 if i == 0 else 0) | (64 if marker else 0)]) + chunk
                                rtp = struct.pack("!BBHII", 0x80, 96 | (128 if marker else 0), sequence & 65535, timestamp, 7) + payload
                                self.wfile.write(b"$\x00" + struct.pack("!H", len(rtp)) + rtp)
                                sequence += 1
                            self.wfile.flush()
                            index += 1
                            stop.wait(.04)
                    except (BrokenPipeError, ConnectionResetError):
                        pass
                    except Exception as error:
                        failures.append(repr(error))
            camera = TCP(("127.0.0.1", 0), Camera)
            camera_thread = threading.Thread(target=camera.serve_forever, daemon=True)
            camera_thread.start()
            enable = []
            class Protect(http.server.BaseHTTPRequestHandler):
                def log_message(self, *args):
                    pass
                def response(self, status, body):
                    encoded = json.dumps(body).encode()
                    self.send_response(status)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(encoded)))
                    self.end_headers()
                    self.wfile.write(encoded)
                def do_GET(self):
                    if self.path.endswith("/snapshot?highQuality=true"):
                        self.send_response(200)
                        self.send_header("Content-Type", "image/jpeg")
                        self.send_header("Content-Length", str(len(jpeg)))
                        self.end_headers()
                        self.wfile.write(jpeg)
                        return
                    self.response(200 if self.path.endswith("/cameras/cam") else 404, {"id": "cam", "state": "CONNECTED"})
                def do_POST(self):
                    body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                    enable.append((self.path, self.headers.get("X-API-KEY"), body))
                    self.response(200, {"high": f"rtsp://127.0.0.1:{camera.server_address[1]}/live"})
            subprocess.run(["openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes", "-keyout", str(folder / "key.pem"), "-out", str(folder / "cert.pem"), "-subj", "/CN=localhost", "-days", "1"], check=True, capture_output=True)
            console = HTTP(("127.0.0.1", 0), Protect)
            tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            tls.minimum_version = ssl.TLSVersion.TLSv1_3
            tls.load_cert_chain(str(folder / "cert.pem"), str(folder / "key.pem"))
            console.socket = tls.wrap_socket(console.socket, server_side=True)
            console_thread = threading.Thread(target=console.serve_forever, daemon=True)
            console_thread.start()
            app = "com.stulp.unifi"
            path = folder / "document.json"
            path.write_text(json.dumps({"version": 2, "apps": [{"id": app, "enabled": True}], "devices": [{"id": "camera", "appId": app, "driverId": "camera", "name": "Synthetic camera", "data": {"id": "cam"}, "capabilities": ["alarm_motion"]}], "appSettings": {app: {"host": "127.0.0.1", "port": console.server_address[1], "apiKey": "synthetic-key"}}, "system": {"attachSecret": "integration-secret"}}))
            chain = None
            try:
                identity = None
                if controller_tls:
                    identity = folder / "controller-tls"
                    identity.mkdir()
                    certificate(identity)
                chain = Chain(path, app, "stulp-unifi", tls=identity)
                deadline = time.monotonic() + 10
                while json.loads(chain.get("/api/manager/apps/app/" + app))["state"] != "running":
                    self.assertLess(time.monotonic(), deadline)
                    time.sleep(.02)
                media = json.loads(chain.get("/api/stulp/devices/camera/media"))
                self.assertEqual([(v["slot"], v["kind"]) for v in media], [("snapshot", "image"), ("live", "video")])
                picture = chain.get("/api/stulp/devices/camera/media/snapshot/stream?kind=image")
                self.assertEqual(picture, jpeg)
                route = chain.base + "/api/stulp/devices/camera/media/live/stream"
                with chain.http.open(route, timeout=15) as response:
                    self.assertIn("avc1.", response.headers["Content-Type"])
                    self.assertNotIn("Location", response.headers)
                    data = bytearray()
                    count = 0
                    while count < 25:
                        header = response.read(8)
                        self.assertEqual(len(header), 8)
                        size, kind = struct.unpack("!I4s", header)
                        self.assertGreaterEqual(size, 8)
                        self.assertLess(size, 8 << 20)
                        body = response.read(size-8)
                        self.assertEqual(len(body), size-8)
                        data.extend(header + body)
                        if kind == b"mdat":
                            count += 1
                    self.assertTrue(json.loads(chain.get("/api/stulp/health"))["ok"])
                output = folder / "proxied.mp4"
                output.write_bytes(data)
                decode = subprocess.run(["ffmpeg", "-v", "warning", "-i", str(output), "-f", "null", "-"], capture_output=True)
                self.assertEqual(decode.returncode, 0, decode.stderr.decode())
                self.assertEqual(decode.stderr, b"")
                probe = json.loads(subprocess.check_output(["ffprobe", "-v", "error", "-count_frames", "-show_entries", "stream=codec_name,width,height,nb_read_frames", "-of", "json", str(output)]))
                self.assertEqual(probe["streams"][0]["nb_read_frames"], "25")
                self.assertEqual(methods, ["DESCRIBE", "SETUP", "PLAY"])
                self.assertEqual(enable, [("/proxy/protect/integration/v1/cameras/cam/rtsps-stream", "synthetic-key", {"qualities": ["high"]})])
                self.assertFalse(failures)
            except Exception:
                if chain:
                    chain.logs.seek(0)
                    print(chain.logs.read().decode())
                raise
            finally:
                stop.set()
                if chain:
                    chain.close()
                console.shutdown(); console.server_close(); console_thread.join(timeout=2)
                camera.shutdown(); camera.server_close(); camera_thread.join(timeout=2)

if __name__ == "__main__":
    unittest.main()
