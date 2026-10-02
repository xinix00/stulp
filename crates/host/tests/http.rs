//! Echte socket- en herstarttest zonder de lokale Stulp-configuratie te openen.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
use std::{
    io::{BufRead, BufReader, Read, Write},
    net::TcpStream,
    path::PathBuf,
    process::{Child, Command, Stdio},
};

struct Server {
    child: Child,
    address: String,
    attach: String,
}
impl Server {
    fn start(path: &std::path::Path) -> Self {
        let mut child = Command::new(env!("CARGO_BIN_EXE_stulp-host"))
            .args(["--document"])
            .arg(path)
            .args([
                "serve",
                "--listen",
                "127.0.0.1:0",
                "--token",
                "test-key",
                "--attach-port",
                "127.0.0.1:0",
                "--attach-plaintext",
            ])
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .unwrap();
        let mut line = String::new();
        let mut output = BufReader::new(child.stdout.take().unwrap());
        output.read_line(&mut line).unwrap();
        let address = line
            .trim()
            .strip_prefix("[stulp:listening] http://")
            .unwrap()
            .to_string();
        line.clear();
        output.read_line(&mut line).unwrap();
        let attach = line
            .trim()
            .strip_prefix("[stulp:attach] tcp://")
            .unwrap()
            .to_string();
        Self {
            child,
            address,
            attach,
        }
    }
    fn request(&self, method: &str, path: &str, cookie: &str, body: &str) -> (u16, String, String) {
        let mut conn = TcpStream::connect(&self.address).unwrap();
        conn.set_read_timeout(Some(std::time::Duration::from_secs(5)))
            .unwrap();
        write!(conn,"{method} {path} HTTP/1.1\r\nHost: {}\r\nConnection: close\r\nCookie: {cookie}\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n{body}",self.address,body.len()).unwrap();
        let mut bytes = Vec::new();
        conn.read_to_end(&mut bytes).unwrap();
        let answer = String::from_utf8(bytes).unwrap();
        let (headers, body) = answer.split_once("\r\n\r\n").unwrap();
        let status = headers.split_whitespace().nth(1).unwrap().parse().unwrap();
        (status, headers.to_string(), body.to_string())
    }
    fn cookie(&self) -> String {
        let (status, headers, body) = self.request("GET", "/test-key", "", "");
        assert_eq!(status, 200);
        assert!(body.contains("<!DOCTYPE html>") || body.contains("<!doctype html>"));
        headers
            .lines()
            .find_map(|line| line.strip_prefix("Set-Cookie: "))
            .unwrap()
            .split(';')
            .next()
            .unwrap()
            .to_string()
    }
}
impl Drop for Server {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}
struct Temp(PathBuf);
impl Temp {
    fn new() -> Self {
        static NEXT: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
        let n = NEXT.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        let p = std::env::temp_dir().join(format!("stulp-rust-test-{}-{n}", std::process::id()));
        std::fs::create_dir(&p).unwrap();
        Self(p)
    }
}
impl Drop for Temp {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

fn read_frame(stream: &mut TcpStream) -> stulp_core::json::Value {
    let mut prefix = [0; 4];
    stream.read_exact(&mut prefix).unwrap();
    let length = u32::from_be_bytes(prefix) as usize;
    assert!(length <= stulp_protocol::MAX_FRAME);
    let mut body = vec![0; length];
    stream.read_exact(&mut body).unwrap();
    stulp_core::json::parse(&body).unwrap()
}
fn send_frame(stream: &mut TcpStream, frame: &stulp_core::json::Value) {
    stream
        .write_all(&stulp_protocol::encode(frame).unwrap())
        .unwrap();
}

#[test]
fn matter_migration_runs_only_after_authorized_enabled_attach_before_welcome() {
    use stulp_core::json::{self, Value};
    use stulp_protocol::{
        Frame,
        token::{self, Direction},
    };
    const APP: &str = "com.stulp.matter";
    for (enabled, valid_proof) in [(false, true), (true, false), (true, true)] {
        let temp = Temp::new();
        let path = temp.0.join("document.json");
        let source = format!(
            r#"{{"version":2,"apps":[{{"id":"{APP}","enabled":{enabled}}}],"devices":[
            {{"id":"lamp","appId":"{APP}","driverId":"matter","name":"Lamp","class":"light","data":{{"id":1}},"capabilities":["onoff"],"store":{{"matter.nodeId":"1","matter.endpoint":1}}}},
            {{"id":"sensor","appId":"{APP}","driverId":"matter","name":"Sensor","class":"sensor","data":{{"id":2}},"capabilities":["measure_temperature"],"store":{{"matter.nodeId":"1","matter.endpoint":2}}}}
        ],"system":{{"attachSecret":"test-secret"}}}}"#
        );
        std::fs::write(&path, &source).unwrap();
        let server = Server::start(&path);
        let mut stream = TcpStream::connect(&server.attach).unwrap();
        stream
            .set_read_timeout(Some(std::time::Duration::from_secs(5)))
            .unwrap();
        let challenge = read_frame(&mut stream);
        let proof = token::proof(
            &token::token(if valid_proof { "test-secret" } else { "wrong" }, APP).unwrap(),
            Direction::App,
            json::text(&challenge, "nonce"),
            APP,
        )
        .unwrap();
        send_frame(
            &mut stream,
            &json::fields(&[
                ("appId", json::string(APP).unwrap()),
                ("protocol", Value::uint(1)),
                ("nonce", json::string("client-nonce").unwrap()),
                ("proof", json::string(&proof).unwrap()),
                (
                    "manifest",
                    json::parse(include_bytes!("../../../plugins/matter/app.json")).unwrap(),
                ),
            ])
            .unwrap(),
        );
        if !enabled || !valid_proof {
            let mut response = Vec::new();
            stream.read_to_end(&mut response).unwrap();
            assert_eq!(std::fs::read_to_string(&path).unwrap(), source);
            continue;
        }
        assert!(json::boolean(&read_frame(&mut stream), "ok"));
        send_frame(
            &mut stream,
            &Frame::request(
                1,
                "hello",
                &json::fields(&[("protocol", Value::uint(1))]).unwrap(),
            )
            .unwrap(),
        );
        let welcome = read_frame(&mut stream);
        let devices = json::get(json::get(&welcome, "r").unwrap(), "devices").unwrap();
        assert!(json::get(devices, "sensor").is_none());
        let lamp = json::get(devices, "lamp").unwrap();
        assert_eq!(json::array(lamp, "capabilities").len(), 2);
        for method in ["app.init", "driver.init", "device.init"] {
            let request = read_frame(&mut stream);
            assert_eq!(json::text(&request, "m"), method);
            if method == "device.init" {
                assert_eq!(
                    json::text(json::get(&request, "p").unwrap(), "deviceId"),
                    "lamp"
                );
            }
            send_frame(
                &mut stream,
                &Frame::response(json::uint(&request, "id"), Ok(Value::Null)).unwrap(),
            );
        }
        let disk = json::parse(&std::fs::read(&path).unwrap()).unwrap();
        assert_eq!(json::array(&disk, "devices").len(), 1);
    }
}

#[test]
fn attach_app_updates_device_before_ack_and_http_invokes_callback() {
    use stulp_core::json::{self, TryClone, Value};
    use stulp_protocol::{
        Frame,
        token::{self, Direction},
    };
    let temp = Temp::new();
    let path = temp.0.join("document.json");
    std::fs::write(&path,br#"{"version":2,"apps":[{"id":"test.app","enabled":true}],"devices":[{"id":"lamp","appId":"test.app","driverId":"lamp","name":"Test lamp","data":{},"capabilities":["onoff"]}],"system":{"attachSecret":"test-secret"}}"#).unwrap();
    let server = Server::start(&path);
    let cookie = server.cookie();
    let mut stream = TcpStream::connect(&server.attach).unwrap();
    stream
        .set_read_timeout(Some(std::time::Duration::from_secs(5)))
        .unwrap();
    let hello = read_frame(&mut stream);
    let token = token::token("test-secret", "test.app").unwrap();
    let proof = token::proof(
        &token,
        Direction::App,
        json::text(&hello, "nonce"),
        "test.app",
    )
    .unwrap();
    let manifest =
        json::parse(br#"{"id":"test.app","version":"1.0","sdk":3,"drivers":[{"id":"lamp"}]}"#)
            .unwrap();
    let greeting = json::fields(&[
        ("appId", json::string("test.app").unwrap()),
        ("protocol", Value::uint(1)),
        ("nonce", json::string("client-nonce").unwrap()),
        ("proof", json::string(&proof).unwrap()),
        ("manifest", manifest),
    ])
    .unwrap();
    send_frame(&mut stream, &greeting);
    let reply = read_frame(&mut stream);
    assert!(json::boolean(&reply, "ok"));
    assert_eq!(
        json::text(&reply, "proof"),
        token::proof(&token, Direction::Stulp, "client-nonce", "test.app").unwrap()
    );
    send_frame(
        &mut stream,
        &Frame::request(
            1,
            "hello",
            &json::fields(&[("protocol", Value::uint(1))]).unwrap(),
        )
        .unwrap(),
    );
    let welcome = read_frame(&mut stream);
    assert_eq!(json::uint(&welcome, "id"), 1);
    for method in ["app.init", "driver.init", "device.init"] {
        let request = read_frame(&mut stream);
        assert_eq!(json::text(&request, "m"), method);
        send_frame(
            &mut stream,
            &Frame::response(json::uint(&request, "id"), Ok(Value::Null)).unwrap(),
        );
    }
    let params =
        json::parse(br#"{"deviceId":"lamp","field":"state","patch":{"onoff":true}}"#).unwrap();
    send_frame(
        &mut stream,
        &Frame::request(2, "device.merge", &params).unwrap(),
    );
    let event = read_frame(&mut stream);
    assert_eq!(json::text(&event, "m"), "state.device");
    let ack = read_frame(&mut stream);
    assert_eq!(json::text(&ack, "t"), "res");
    assert_eq!(json::uint(&ack, "id"), 2);
    let (status, _, body) = server.request("GET", "/api/manager/devices/device/lamp", &cookie, "");
    assert_eq!(status, 200);
    assert!(body.contains("\"value\":true"));
    assert!(
        !std::fs::read_to_string(&path)
            .unwrap()
            .contains("onoff\":true")
    );
    std::thread::scope(|scope| {
        let http = scope.spawn(|| {
            server.request(
                "PUT",
                "/api/manager/devices/device/lamp/capability/onoff",
                &cookie,
                r#"{"value":false}"#,
            )
        });
        let call = read_frame(&mut stream);
        assert_eq!(json::text(&call, "m"), "capability.invoke");
        assert_eq!(
            json::get(json::get(&call, "p").unwrap(), "value")
                .unwrap()
                .try_clone()
                .unwrap(),
            Value::Bool(false)
        );
        send_frame(
            &mut stream,
            &Frame::response(json::uint(&call, "id"), Ok(Value::Bool(true))).unwrap(),
        );
        assert_eq!(http.join().unwrap().0, 200);
    });
    // De bestaande Test-knop moet werkelijk de app aanroepen en historie opslaan.
    let (status, _, body) = server.request("POST", "/api/manager/flow/flow", &cookie,
        r#"{"name":"Callback flow","enabled":false,"nodes":[{"id":"t","step":{"appId":"stulp","cardType":"trigger","cardId":"time_at"}},{"id":"a","step":{"appId":"test.app","cardType":"action","cardId":"send","args":{"message":"hello"}}}],"edges":[{"from":"t","to":"a"}]}"#);
    assert_eq!(status, 201, "{body}");
    let definition = json::parse(body.as_bytes()).unwrap();
    let id = json::text(&definition, "id");
    std::thread::scope(|scope| {
        let http = scope
            .spawn(|| server.request("POST", &format!("/api/stulp/flows/{id}/run"), &cookie, ""));
        let call = read_frame(&mut stream);
        assert_eq!(json::text(&call, "m"), "flow.run");
        let params = json::get(&call, "p").unwrap();
        assert_eq!(json::text(params, "kind"), "action");
        assert_eq!(json::text(params, "id"), "send");
        // HTTP blijft werken terwijl de Flow op zijn app wacht.
        assert_eq!(
            server
                .request("GET", "/api/stulp/manage/bootstrap", &cookie, "")
                .0,
            200
        );
        send_frame(
            &mut stream,
            &Frame::response(json::uint(&call, "id"), Ok(Value::Bool(true))).unwrap(),
        );
        let (status, _, body) = http.join().unwrap();
        assert_eq!(status, 200, "{body}");
        let result = json::parse(body.as_bytes()).unwrap();
        assert!(json::boolean(&result, "success"));
        assert_eq!(json::array(&result, "actions").len(), 1);
    });
    assert_eq!(
        server
            .request(
                "PUT",
                "/api/manager/apps/app/test.app/setting/theme",
                &cookie,
                r#"{"value":"dark"}"#
            )
            .0,
        200
    );
    let update = read_frame(&mut stream);
    assert_eq!(json::text(&update, "m"), "state.settings");
    assert_eq!(
        json::text(json::get(&update, "p").unwrap(), "theme"),
        "dark"
    );
    assert_eq!(
        server
            .request("GET", "/api/manager/apps/app/test.app", &cookie, "")
            .0,
        200
    );
    let (status, _, drivers) = server.request("GET", "/api/manager/drivers/driver", &cookie, "");
    assert_eq!(status, 200);
    assert!(drivers.contains("stulp:app:test.app:lamp"));
    std::thread::scope(|scope| {
        let request = scope.spawn(|| {
            server.request(
                "GET",
                "/api/stulp/apps/test.app/drivers/lamp/pair/devices",
                &cookie,
                "",
            )
        });
        let call = read_frame(&mut stream);
        assert_eq!(json::text(&call, "m"), "pair.list");
        send_frame(
            &mut stream,
            &Frame::response(json::uint(&call, "id"), Ok(Value::Array(Vec::new()))).unwrap(),
        );
        assert_eq!(request.join().unwrap().0, 200);
    });
    // Een mislukte adoptie ruimt duurzame data op en houdt de app bruikbaar.
    let paired_id = std::thread::scope(|scope| {
        let mut last_id = String::new();
        for fail in [true, false] {
            let request = scope.spawn(|| {
                server.request(
                    "POST",
                    "/api/stulp/apps/test.app/drivers/lamp/pair/devices",
                    &cookie,
                    r#"{"name":"New lamp","data":{"serial":"pair-test"},"capabilities":["onoff"]}"#,
                )
            });
            let mut snapshot_seen = false;
            let mut initialized_driver = false;
            loop {
                let call = read_frame(&mut stream);
                match json::text(&call, "m") {
                    "state.device" => {
                        let params = json::get(&call, "p").unwrap();
                        let device = json::get(params, "device").unwrap();
                        if json::text(device, "name") == "New lamp" {
                            snapshot_seen = true;
                            last_id = json::text(params, "deviceId").to_string();
                        }
                    }
                    "driver.init" => {
                        assert!(snapshot_seen);
                        assert!(!initialized_driver, "no duplicate driver init");
                        initialized_driver = true;
                        send_frame(
                            &mut stream,
                            &Frame::response(json::uint(&call, "id"), Ok(Value::Null)).unwrap(),
                        );
                    }
                    "device.init" => {
                        assert!(snapshot_seen && initialized_driver);
                        let result = if fail {
                            Err("synthetic initialization failure")
                        } else {
                            Ok(Value::Null)
                        };
                        send_frame(
                            &mut stream,
                            &Frame::response(json::uint(&call, "id"), result).unwrap(),
                        );
                        break;
                    }
                    "$appproto.ping" => send_frame(
                        &mut stream,
                        &Frame::response(json::uint(&call, "id"), Ok(Value::Null)).unwrap(),
                    ),
                    other => panic!("unexpected adoption callback {other}"),
                }
            }
            let (status, _, body) = request.join().unwrap();
            assert_eq!(status, if fail { 502 } else { 201 }, "{body}");
            if fail {
                assert!(body.contains("synthetic initialization failure"));
            }
            let saved = json::parse(&std::fs::read(&path).unwrap()).unwrap();
            assert_eq!(
                json::array(&saved, "devices").len(),
                if fail { 1 } else { 2 }
            );
        }
        last_id
    });
    // Instellingen worden pas opgeslagen na bevestiging; gelijktijdige bediening blijft bereikbaar.
    let device_route = format!("/api/manager/devices/device/{paired_id}");
    let settings_route = format!("{device_route}/settings");
    assert_eq!(server.request("PUT", &settings_route, &cookie, "[]").0, 400);
    for reject in [true, false] {
        std::thread::scope(|scope| {
            let request = scope.spawn(|| {
                server.request(
                    "PUT",
                    &settings_route,
                    &cookie,
                    r#"{"address":"192.0.2.8"}"#,
                )
            });
            let call = loop {
                let f = read_frame(&mut stream);
                match json::text(&f, "m") {
                    "device.settings" => break f,
                    "state.device" => (),
                    "$appproto.ping" => send_frame(
                        &mut stream,
                        &Frame::response(json::uint(&f, "id"), Ok(Value::Null)).unwrap(),
                    ),
                    other => panic!("unexpected settings callback {other}"),
                }
            };
            let params = json::get(&call, "p").unwrap();
            assert_eq!(json::text(params, "deviceId"), paired_id);
            assert_eq!(
                json::text(json::get(params, "settings").unwrap(), "address"),
                "192.0.2.8"
            );
            let body = server.request("GET", &device_route, &cookie, "").2;
            assert!(!body.contains("192.0.2.8"));
            assert_eq!(server.request("DELETE", &device_route, &cookie, "").0, 409);
            assert_eq!(
                server
                    .request(
                        "PUT",
                        &settings_route,
                        &cookie,
                        r#"{"address":"192.0.2.9"}"#
                    )
                    .0,
                409
            );
            assert_eq!(
                server
                    .request(
                        "PUT",
                        &device_route,
                        &cookie,
                        r#"{"name":"Renamed while validating"}"#
                    )
                    .0,
                200
            );
            send_frame(
                &mut stream,
                &Frame::response(
                    json::uint(&call, "id"),
                    if reject {
                        Err("synthetic invalid setting")
                    } else {
                        Ok(Value::Null)
                    },
                )
                .unwrap(),
            );
            let (status, _, body) = request.join().unwrap();
            assert_eq!(status, if reject { 502 } else { 200 }, "{body}");
            if reject {
                assert!(body.contains("synthetic invalid setting"));
            }
            let stored = json::parse(&std::fs::read(&path).unwrap()).unwrap();
            let device = json::array(&stored, "devices")
                .iter()
                .find(|d| json::text(d, "id") == paired_id)
                .unwrap();
            assert_eq!(json::text(device, "name"), "Renamed while validating");
            assert_eq!(
                json::text(json::get(device, "settings").unwrap(), "address"),
                if reject { "" } else { "192.0.2.8" }
            );
        });
    }
    // Een ongewijzigde patch veroorzaakt geen tweede apparaatcommando.
    assert_eq!(
        server
            .request(
                "PUT",
                &settings_route,
                &cookie,
                r#"{"address":"192.0.2.8"}"#
            )
            .0,
        200
    );
    // Verwijderen vraagt eerst de plugin, maar opruimfouten blokkeren de gebruiker niet.
    let route = format!("/api/manager/devices/device/{paired_id}");
    std::thread::scope(|scope| {
        let request = scope.spawn(|| server.request("DELETE", &route, &cookie, ""));
        loop {
            let call = read_frame(&mut stream);
            match json::text(&call, "m") {
                "state.device" => (),
                "$appproto.ping" => send_frame(
                    &mut stream,
                    &Frame::response(json::uint(&call, "id"), Ok(Value::Null)).unwrap(),
                ),
                "device.delete" => {
                    assert_eq!(server.request("GET", &route, &cookie, "").0, 200);
                    send_frame(
                        &mut stream,
                        &Frame::response(json::uint(&call, "id"), Err("synthetic offline device"))
                            .unwrap(),
                    );
                    break;
                }
                other => panic!("unexpected deletion callback {other}"),
            }
        }
        assert_eq!(request.join().unwrap().0, 200);
        assert_eq!(server.request("GET", &route, &cookie, "").0, 404);
        // Haal de uiteindelijke verwijderingssnapshot op vóór de volgende proef.
        let deletion = read_frame(&mut stream);
        assert_eq!(json::text(&deletion, "m"), "state.device");
        assert_eq!(
            json::get(json::get(&deletion, "p").unwrap(), "device"),
            Some(&Value::Null)
        );
    });
    // Notify krijgt een tijdelijk relatief adres; alleen de fetch vraagt de camera om beeld.
    send_frame(&mut stream,&Frame::request(20,"media.register",&json::parse(br#"{"deviceId":"lamp","media":[{"slot":"snapshot","kind":"image","title":"Test image"}]}"#).unwrap()).unwrap());
    assert_eq!(json::uint(&read_frame(&mut stream), "id"), 20);
    send_frame(
        &mut stream,
        &Frame::request(21, "images.list", &json::object()).unwrap(),
    );
    let listed = read_frame(&mut stream);
    assert_eq!(
        json::get(&listed, "r").unwrap().as_array().unwrap().len(),
        1
    );
    send_frame(
        &mut stream,
        &Frame::request(
            22,
            "image.url",
            &json::parse(br#"{"deviceId":"lamp","slot":""}"#).unwrap(),
        )
        .unwrap(),
    );
    let ticket = read_frame(&mut stream);
    let ticket = json::text(json::get(&ticket, "r").unwrap(), "url").to_string();
    assert!(ticket.starts_with("/image/"));
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let image_address = format!("http://{}/private", listener.local_addr().unwrap());
    std::thread::scope(|scope| {
        let source=scope.spawn(|| {
            let (mut socket,_)=listener.accept().unwrap();
            socket.set_read_timeout(Some(std::time::Duration::from_secs(5))).unwrap();
            {let mut read=BufReader::new(&socket);loop {let mut line=String::new();read.read_line(&mut line).unwrap();if line=="\r\n" {break;}}}
            socket.write_all(b"HTTP/1.1 200 OK\r\nContent-Type: image/jpeg\r\nContent-Length: 14\r\nConnection: close\r\n\r\nsynthetic-jpeg").unwrap();
        });
        let fetch = scope.spawn(|| server.request("GET", &ticket, "", ""));
        let call = read_frame(&mut stream);
        assert_eq!(json::text(&call, "m"), "video.resolve");
        assert_eq!(json::text(json::get(&call, "p").unwrap(), "kind"), "image");
        let answer = json::fields(&[
            ("url", json::string(&image_address).unwrap()),
            ("contentType", json::string("image/jpeg").unwrap()),
        ])
        .unwrap();
        send_frame(
            &mut stream,
            &Frame::response(json::uint(&call, "id"), Ok(answer)).unwrap(),
        );
        let (status, _, body) = fetch.join().unwrap();
        assert_eq!(status, 200, "{body}");
        assert!(body.contains("synthetic-jpeg"));
        source.join().unwrap();
    });
    assert_eq!(
        server.request("GET", "/image/does-not-exist", "", "").0,
        404
    );
    // De controller antwoordt meteen als een callback zijn verbinding verliest.
    std::thread::scope(|scope| {
        let request = scope
            .spawn(|| server.request("GET", "/api/stulp/apps/test.app/registrations", &cookie, ""));
        let call = read_frame(&mut stream);
        assert_eq!(json::text(&call, "m"), "registrations");
        stream.shutdown(std::net::Shutdown::Both).unwrap();
        let (status, _, body) = request.join().unwrap();
        assert_eq!(status, 502);
        assert!(body.contains("disconnected"));
    });
    let bytes = std::fs::read(&path).unwrap();
    let saved = json::parse(&bytes).unwrap();
    assert!(!json::text(&json::array(&saved, "flows")[0], "lastRunAt").is_empty());
}

#[test]
fn original_ui_auth_crud_and_persistence_over_real_http() {
    let temp = Temp::new();
    let path = temp.0.join("document.json");
    let server = Server::start(&path);
    assert_eq!(
        server
            .request("GET", "/api/stulp/manage/bootstrap", "", "")
            .0,
        401
    );
    assert_eq!(server.request("GET", "/assets/style.css", "", "").0, 200);
    assert_eq!(server.request("GET", "/mcp/incorrect", "", "").0, 404);
    let cookie = server.cookie();
    let (status, _, body) = server.request(
        "POST",
        "/api/stulp/device-groups",
        &cookie,
        r#"{"name":"Keuken"}"#,
    );
    assert_eq!(status, 201, "{body}");
    let group = stulp_core::json::parse(body.as_bytes()).unwrap();
    let id = stulp_core::json::text(&group, "id");
    assert_eq!(
        server
            .request(
                "PUT",
                &format!("/api/stulp/device-groups/{id}"),
                &cookie,
                r#"{"name":"Woonkamer"}"#
            )
            .0,
        200
    );
    drop(server);
    let server = Server::start(&path);
    let cookie = server.cookie();
    let (status, _, body) = server.request("GET", "/api/stulp/device-groups", &cookie, "");
    assert_eq!(status, 200);
    assert!(body.contains("Woonkamer"));
    assert!(!body.contains("Keuken"));
    assert_eq!(
        server
            .request(
                "DELETE",
                &format!("/api/stulp/device-groups/{id}"),
                &cookie,
                ""
            )
            .0,
        200
    );
    use std::os::unix::fs::PermissionsExt;
    assert_eq!(
        std::fs::metadata(&path).unwrap().permissions().mode() & 0o777,
        0o600
    );
    let mut files: Vec<_> = std::fs::read_dir(&temp.0)
        .unwrap()
        .map(|entry| entry.unwrap().file_name())
        .collect();
    files.sort();
    assert_eq!(
        files,
        vec![
            std::ffi::OsString::from("document.json"),
            std::ffi::OsString::from("document.json.lock")
        ]
    );
}

#[test]
fn live_stream_delivers_mutations_without_reloading_or_blocking_http() {
    let temp = Temp::new();
    let path = temp.0.join("document.json");
    let server = Server::start(&path);
    let cookie = server.cookie();
    let mut stream = TcpStream::connect(&server.address).unwrap();
    stream
        .set_read_timeout(Some(std::time::Duration::from_secs(5)))
        .unwrap();
    write!(
        stream,
        "GET /api/stulp/events?view=overview HTTP/1.1\r\nHost: {}\r\nCookie: {cookie}\r\n\r\n",
        server.address
    )
    .unwrap();
    let mut reader = BufReader::new(stream);
    let mut line = String::new();
    reader.read_line(&mut line).unwrap();
    assert!(line.contains("200"));
    loop {
        line.clear();
        reader.read_line(&mut line).unwrap();
        if line == "\r\n" {
            break;
        }
    }
    assert_eq!(
        server
            .request(
                "POST",
                "/api/stulp/device-groups",
                &cookie,
                r#"{"name":"Live room"}"#
            )
            .0,
        201
    );
    let mut found = false;
    for _ in 0..20 {
        line.clear();
        reader.read_line(&mut line).unwrap();
        if line.contains("\"type\":\"group.create\"") {
            found = true;
            break;
        }
    }
    assert!(found, "SSE did not deliver the mutation");
    assert_eq!(
        server
            .request("GET", "/api/stulp/manage/bootstrap", &cookie, "")
            .0,
        200
    );
}
