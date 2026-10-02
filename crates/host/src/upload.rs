//! De host-upload streamt maximaal 512 MiB naar private staging vóór ZIP-validatie.
//! Lean HTTP begrenst gewone bodies op 1 MiB; deze route heeft zijn eigen framing.
use super::{Call, Work};
use std::{
    io::{self, BufRead, BufReader, Write},
    path::Path,
    sync::mpsc::{self, SyncSender},
    time::{Duration, Instant},
};
use stulp_core::json;
use stulp_web::{Body, Environment as _, Request, Response};
const MAX: u64 = 512 << 20;
fn bad(s: &'static str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, s)
}
pub(super) fn matches(stream: &mut stulp_transport::Stream<'_>) -> io::Result<bool> {
    let prefix = b"POST /api/stulp/restore";
    let start = Instant::now();
    let mut bytes = [0; 32];
    loop {
        let n = stream.peek(&mut bytes)?;
        if n == 0 {
            return Ok(false);
        }
        let shared = n.min(prefix.len());
        if bytes[..shared] != prefix[..shared] {
            return Ok(false);
        }
        if n > prefix.len() {
            return Ok(matches!(bytes[prefix.len()], b' ' | b'?'));
        }
        if start.elapsed() > Duration::from_secs(15) {
            return Err(bad("incomplete upload request line"));
        }
        std::thread::sleep(Duration::from_millis(1));
    }
}
fn line(input: &mut impl BufRead, budget: &mut usize) -> io::Result<String> {
    let mut bytes = Vec::new();
    loop {
        let available = input.fill_buf()?;
        if available.is_empty() {
            return Err(bad("incomplete upload headers"));
        }
        let n = available
            .iter()
            .position(|b| *b == b'\n')
            .map_or(available.len(), |i| i + 1);
        if n > *budget || bytes.len() + n > 8192 {
            return Err(bad("upload header limit exceeded"));
        }
        bytes.try_reserve(n).map_err(io::Error::other)?;
        bytes.extend_from_slice(&available[..n]);
        input.consume(n);
        *budget -= n;
        if bytes.last() == Some(&b'\n') {
            break;
        }
    }
    if !bytes.ends_with(b"\r\n") {
        return Err(bad("invalid upload header newline"));
    }
    bytes.truncate(bytes.len() - 2);
    String::from_utf8(bytes).map_err(|_| bad("upload headers not UTF-8"))
}
fn headers(input: &mut impl BufRead) -> io::Result<(Request, Option<u64>, bool, bool)> {
    let mut budget = 32768;
    let first = line(input, &mut budget)?;
    let parts: Vec<_> = first.split(' ').collect();
    if parts.len() != 3 || parts[0] != "POST" || !matches!(parts[2], "HTTP/1.1" | "HTTP/1.0") {
        return Err(bad("invalid upload request line"));
    }
    let (path, query) = parts[1].split_once('?').unwrap_or((parts[1], ""));
    if path != "/api/stulp/restore" {
        return Err(bad("invalid upload path"));
    }
    let mut values = json::object();
    loop {
        let text = line(input, &mut budget)?;
        if text.is_empty() {
            break;
        }
        let (key, value) = text
            .split_once(':')
            .ok_or_else(|| bad("invalid upload header"))?;
        if key.is_empty()
            || !key
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b"!#$%&'*+-.^_`|~".contains(&b))
            || value.bytes().any(|b| b < 32 && b != b'\t' || b == 127)
        {
            return Err(bad("invalid upload header"));
        }
        let key = key.to_ascii_lowercase();
        if json::get(&values, &key).is_some() {
            return Err(bad("duplicate upload header"));
        }
        json::set(
            &mut values,
            &key,
            json::string(value.trim()).map_err(io::Error::other)?,
        )
        .map_err(io::Error::other)?;
    }
    if json::text(&values, "host").is_empty() {
        return Err(bad("upload Host missing"));
    }
    let length = match json::get(&values, "content-length") {
        None => None,
        Some(v) => {
            let s = v.as_str().unwrap_or("");
            if s.is_empty() || !s.bytes().all(|b| b.is_ascii_digit()) {
                return Err(bad("invalid upload length"));
            }
            Some(s.parse::<u64>().map_err(|_| bad("invalid upload length"))?)
        }
    };
    let encoding = json::text(&values, "transfer-encoding");
    let chunked = encoding.eq_ignore_ascii_case("chunked");
    if !(chunked && length.is_none() || encoding.is_empty() && length.is_some()) {
        return Err(bad("ambiguous upload framing"));
    }
    let expect = json::text(&values, "expect");
    if !expect.is_empty() && !expect.eq_ignore_ascii_case("100-continue") {
        return Err(bad("unsupported upload expectation"));
    }
    let expect = !expect.is_empty();
    let copy = |s: &str| json::copy(s).map_err(io::Error::other);
    Ok((
        Request {
            method: copy("POST")?,
            path: copy(path)?,
            query: copy(query)?,
            host: copy(json::text(&values, "host"))?,
            origin: copy(json::text(&values, "origin"))?,
            cookie: copy(json::text(&values, "cookie"))?,
            body: Vec::new(),
            headers: values,
        },
        length,
        chunked,
        expect,
    ))
}
fn transfer(
    input: &mut impl BufRead,
    out: &mut impl Write,
    length: Option<u64>,
    chunked: bool,
) -> io::Result<()> {
    let mut total = 0_u64;
    loop {
        let n = if chunked {
            let mut budget = 8192;
            let text = line(input, &mut budget)?;
            let hex = text.split(';').next().unwrap_or("");
            if hex.is_empty() || !hex.bytes().all(|b| b.is_ascii_hexdigit()) {
                return Err(bad("invalid upload chunk"));
            }
            let n = u64::from_str_radix(hex, 16).map_err(|_| bad("upload chunk overflow"))?;
            if n == 0 {
                let mut budget = 32768;
                while !line(input, &mut budget)?.is_empty() {}
                return Ok(());
            }
            n
        } else {
            length.ok_or_else(|| bad("upload length missing"))?
        };
        total = total
            .checked_add(n)
            .filter(|n| *n <= MAX)
            .ok_or_else(|| bad("backup exceeds 512 MiB upload limit"))?;
        let mut left = n;
        let mut buffer = [0; 8192];
        while left > 0 {
            let size = (left.min(buffer.len() as u64)) as usize;
            let n = input.read(&mut buffer[..size])?;
            if n == 0 {
                return Err(bad("truncated backup upload"));
            }
            out.write_all(&buffer[..n])?;
            left -= n as u64;
        }
        if !chunked {
            return Ok(());
        }
        let mut end = [0; 2];
        input.read_exact(&mut end)?;
        if end != *b"\r\n" {
            return Err(bad("invalid upload chunk terminator"));
        }
    }
}
struct Temporary(std::path::PathBuf);
impl Drop for Temporary {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}
fn receive(
    input: &mut BufReader<stulp_transport::Stream<'_>>,
    sender: &SyncSender<Work>,
) -> io::Result<Response> {
    use std::os::unix::fs::{DirBuilderExt, OpenOptionsExt};
    let (request, length, chunked, expect) = headers(input)?;
    if length.is_some_and(|n| n > MAX) {
        return Response::error(413, "backup exceeds 512 MiB upload limit")
            .map_err(io::Error::other);
    }
    let (reply, answers) = mpsc::sync_channel(1);
    sender
        .send(Work::Request(Call {
            request,
            reply: reply.clone(),
        }))
        .map_err(|_| bad("controller stopped"))?;
    let response = answers.recv().map_err(|_| bad("controller stopped"))?;
    let Body::Restore { destination, .. } = response.body else {
        return Ok(response);
    };
    if expect {
        input
            .get_mut()
            .write_all(b"HTTP/1.1 100 Continue\r\n\r\n")?;
    }
    let temporary = std::env::temp_dir().join(format!(
        "stulp-upload-{}",
        crate::Environment.id().map_err(io::Error::other)?
    ));
    std::fs::DirBuilder::new().mode(0o700).create(&temporary)?;
    let temporary = Temporary(temporary);
    let mut file = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(temporary.0.join("upload.zip"))?;
    transfer(input, &mut file, length, chunked)?;
    let prepared = match crate::archive::Prepared::read(&mut file, Path::new(&destination)) {
        Ok(p) => p,
        Err(e) => return Response::error(422, &e.to_string()).map_err(io::Error::other),
    };
    sender
        .send(Work::Restore(prepared, reply))
        .map_err(|_| bad("controller stopped"))?;
    answers.recv().map_err(|_| bad("controller stopped"))
}
pub(super) fn serve(
    stream: stulp_transport::Stream<'_>,
    sender: &SyncSender<Work>,
) -> io::Result<()> {
    let mut input = BufReader::new(stream);
    let result = receive(&mut input, sender);
    let response = match result {
        Ok(r) => r,
        Err(e) => Response::error(400, &e.to_string()).map_err(io::Error::other)?,
    };
    let stream = input.get_mut();
    write!(
        stream,
        "HTTP/1.1 {} Stulp\r\nContent-Type: {}\r\nContent-Length: {}\r\nConnection: close\r\nCache-Control: no-store\r\nX-Content-Type-Options: nosniff\r\n\r\n",
        response.status,
        response.content_type,
        response.body.bytes().len()
    )?;
    stream.write_all(response.body.bytes())?;
    let result = stream.flush();
    stream.close();
    result
}
