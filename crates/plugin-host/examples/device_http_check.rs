//! Alleen synthetische localhost-TLS: wordt gestart door devicecheck.go.
#[path = "../src/device_tls.rs"]
mod device_tls;
#[path = "../src/streams.rs"]
mod streams;
use stulp_sdk::{HttpRequest, StreamCommand, StreamEvent};
fn main() -> Result<(), Box<dyn std::error::Error>> {
    let url = std::env::var("STULP_TEST_DEVICE_URL")?;
    if !url.starts_with("https://127.0.0.1:") {
        return Err("test requires loopback".into());
    }
    let public = hostnet::Http::new();
    if public
        .request_until(
            &hostnet::Call::get(&url, std::time::Duration::from_secs(2)),
            1024,
            std::time::Instant::now() + std::time::Duration::from_secs(2),
        )
        .is_ok()
    {
        return Err("cloud client accepted a self-signed certificate".into());
    }
    for (path, wanted) in [
        ("/httpapi.asp?command=setPlayerCmd:vol:40", 200),
        ("/redirect", 302),
    ] {
        let mut req = HttpRequest::get(&(url.clone() + path)).map_err(|e| e.to_string())?;
        req.device_certificate = true;
        let reply = device_tls::execute(req).map_err(|e| e.to_string())?;
        if reply.status != wanted {
            return Err(format!("unexpected response: {}", reply.status).into());
        }
    }
    let port: u16 = url.rsplit(':').next().ok_or("port missing")?.parse()?;
    let mut stream = streams::Worker::new()?;
    stream.send(StreamCommand::Open {
        id: 1,
        host: "127.0.0.1".into(),
        port,
        tls: true,
        device_certificate: true,
    })?;
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
    let mut bytes = Vec::new();
    loop {
        if std::time::Instant::now() >= deadline {
            return Err("stream deadline".into());
        }
        match stream.poll() {
            Some(StreamEvent::Opened(1)) => stream.send(StreamCommand::Write {
                id: 1,
                bytes: b"GET /stream HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"
                    .to_vec(),
            })?,
            Some(StreamEvent::Data(1, data)) => bytes.extend_from_slice(&data),
            Some(StreamEvent::Closed(1, _)) => break,
            Some(_) => return Err("wrong stream id".into()),
            None => std::thread::sleep(std::time::Duration::from_millis(2)),
        }
    }
    let body_at = bytes
        .windows(4)
        .position(|v| v == b"\r\n\r\n")
        .ok_or("stream HTTP header missing")?
        + 4;
    if bytes.len() - body_at != 40000 || bytes[body_at..].iter().any(|v| *v != 42) {
        return Err("TLS stream lost or changed bytes".into());
    }
    println!("Local device TLS passed; cloud validation retained; redirects not followed.");
    Ok(())
}
