//! Large restore bodies have explicit framing and are authorized before allocation.
use crate::{
    replies::Sender,
    server::{Call, WORK, Work},
};
use alloc::{format, string::String, vec::Vec};
use applib::{appnet::TcpStream, tcp::Stream};
use stulp_controller::Reply;
use stulp_core::{Error, Result, json};
use stulp_web::{Request, Response};
const MAX: usize = 80 << 20;
pub(crate) struct Prefix {
    stream: TcpStream,
    bytes: Vec<u8>,
    at: usize,
}
impl Stream for Prefix {
    async fn read(
        &mut self,
        out: &mut [u8],
    ) -> core::result::Result<usize, applib::appnet::NetError> {
        if self.at < self.bytes.len() {
            let n = out.len().min(self.bytes.len() - self.at);
            out[..n].copy_from_slice(&self.bytes[self.at..self.at + n]);
            self.at += n;
            return Ok(n);
        }
        self.stream.read(out).await
    }
    async fn write(
        &mut self,
        bytes: &[u8],
    ) -> core::result::Result<usize, applib::appnet::NetError> {
        self.stream.write(bytes).await
    }
    fn close(self) -> core::result::Result<(), applib::appnet::NetError> {
        self.stream.close()
    }
}
impl Prefix {
    pub(crate) async fn open(mut stream: TcpStream) -> Result<Self> {
        stream.set_timeout(Some(core::time::Duration::from_secs(15)));
        let mut bytes = Vec::new();
        bytes.try_reserve_exact(1024).map_err(|_| Error::Memory)?;
        bytes.resize(1024, 0);
        let n = stream
            .read(&mut bytes)
            .await
            .map_err(|_| Error::Invalid("request prefix unavailable"))?;
        bytes.truncate(n);
        Ok(Self {
            stream,
            bytes,
            at: 0,
        })
    }
    pub(crate) async fn restore(&mut self) -> Result<bool> {
        let target = b"POST /api/stulp/restore";
        loop {
            let n = self.bytes.len().min(target.len());
            if self.bytes[..n] != target[..n] {
                return Ok(false);
            }
            if self.bytes.len() > target.len() {
                return Ok(matches!(self.bytes[target.len()], b' ' | b'?'));
            }
            self.bytes.try_reserve(1024).map_err(|_| Error::Memory)?;
            let mut buf = [0; 1024];
            let n = self
                .stream
                .read(&mut buf)
                .await
                .map_err(|_| Error::Invalid("request line read failed"))?;
            if n == 0 {
                return Err(Error::Invalid("incomplete request line"));
            }
            self.bytes.extend_from_slice(&buf[..n]);
        }
    }
    pub(crate) fn normal(mut self) -> Self {
        self.stream.set_timeout(None);
        self
    }
    async fn byte(&mut self) -> Result<u8> {
        if self.at == self.bytes.len() {
            self.bytes.resize(1024, 0);
            let n = self
                .stream
                .read(&mut self.bytes)
                .await
                .map_err(|_| Error::Invalid("upload read failed"))?;
            if n == 0 {
                return Err(Error::Invalid("truncated upload"));
            }
            self.bytes.truncate(n);
            self.at = 0;
        }
        let b = self.bytes[self.at];
        self.at += 1;
        Ok(b)
    }
    async fn line(&mut self, budget: &mut usize) -> Result<String> {
        let mut bytes = Vec::new();
        loop {
            if *budget == 0 || bytes.len() >= 8192 {
                return Err(Error::Full);
            }
            let b = self.byte().await?;
            *budget -= 1;
            json::push(&mut bytes, b, 8192)?;
            if b == b'\n' {
                break;
            }
        }
        if !bytes.ends_with(b"\r\n") {
            return Err(Error::Invalid("invalid upload header newline"));
        }
        bytes.truncate(bytes.len() - 2);
        String::from_utf8(bytes).map_err(|_| Error::Invalid("upload header not UTF-8"))
    }
    async fn head(&mut self) -> Result<(Request, Option<usize>, bool)> {
        let mut budget = 32768;
        let first = self.line(&mut budget).await?;
        let mut words = first.split(' ');
        if words.next() != Some("POST") {
            return Err(Error::Invalid("invalid upload method"));
        }
        let target = words
            .next()
            .ok_or(Error::Invalid("upload target missing"))?;
        if !matches!(words.next(), Some("HTTP/1.1" | "HTTP/1.0")) || words.next().is_some() {
            return Err(Error::Invalid("invalid upload request line"));
        }
        let (path, query) = target.split_once('?').unwrap_or((target, ""));
        if path != "/api/stulp/restore" {
            return Err(Error::Invalid("invalid upload path"));
        }
        let mut values = json::object();
        loop {
            let line = self.line(&mut budget).await?;
            if line.is_empty() {
                break;
            }
            let (k, v) = line
                .split_once(':')
                .ok_or(Error::Invalid("invalid upload header"))?;
            if k.is_empty()
                || !k
                    .bytes()
                    .all(|b| b.is_ascii_alphanumeric() || b"!#$%&'*+-.^_`|~".contains(&b))
                || v.bytes().any(|b| b < 32 && b != b'\t' || b == 127)
            {
                return Err(Error::Invalid("invalid upload header"));
            }
            let key = k.to_ascii_lowercase();
            if json::get(&values, &key).is_some() {
                return Err(Error::Invalid("duplicate upload header"));
            }
            json::set(&mut values, &key, json::string(v.trim())?)?;
        }
        let length = match json::get(&values, "content-length") {
            None => None,
            Some(v) => {
                let s = v.as_str().unwrap_or("");
                if s.is_empty() || !s.bytes().all(|b| b.is_ascii_digit()) {
                    return Err(Error::Invalid("invalid upload length"));
                }
                Some(s.parse::<usize>().map_err(|_| Error::Full)?)
            }
        };
        let encoding = json::text(&values, "transfer-encoding");
        if !(encoding.eq_ignore_ascii_case("chunked") && length.is_none()
            || encoding.is_empty() && length.is_some())
        {
            return Err(Error::Invalid("ambiguous upload framing"));
        }
        if length.is_some_and(|n| n > MAX) {
            return Err(Error::Full);
        }
        let expect = json::text(&values, "expect");
        if !expect.is_empty() && !expect.eq_ignore_ascii_case("100-continue") {
            return Err(Error::Invalid("unsupported upload expectation"));
        }
        let expect = !expect.is_empty();
        if json::text(&values, "host").is_empty() {
            return Err(Error::Invalid("upload Host missing"));
        }
        Ok((
            Request {
                method: json::copy("POST")?,
                path: json::copy(path)?,
                query: json::copy(query)?,
                host: json::copy(json::text(&values, "host"))?,
                origin: json::copy(json::text(&values, "origin"))?,
                cookie: json::copy(json::text(&values, "cookie"))?,
                body: Vec::new(),
                headers: values,
            },
            length,
            expect,
        ))
    }
    async fn body(&mut self, length: Option<usize>) -> Result<Vec<u8>> {
        let mut body = Vec::new();
        loop {
            let n = match length {
                Some(n) => n,
                None => {
                    let text = self.line(&mut 8192).await?;
                    let hex = text.split(';').next().unwrap_or("");
                    if hex.is_empty() || !hex.bytes().all(|b| b.is_ascii_hexdigit()) {
                        return Err(Error::Invalid("invalid upload chunk"));
                    }
                    usize::from_str_radix(hex, 16).map_err(|_| Error::Full)?
                }
            };
            if n > MAX.saturating_sub(body.len()) {
                return Err(Error::Full);
            }
            if n == 0 && length.is_none() {
                let mut budget = 32768;
                while !self.line(&mut budget).await?.is_empty() {}
                return Ok(body);
            }
            body.try_reserve(n).map_err(|_| Error::Memory)?;
            let start = body.len();
            body.resize(start + n, 0);
            let mut at = start;
            while at < body.len() {
                let end = body.len().min(at + 65536);
                let n = Stream::read(self, &mut body[at..end])
                    .await
                    .map_err(|_| Error::Invalid("upload body read failed"))?;
                if n == 0 {
                    return Err(Error::Invalid("truncated upload"));
                }
                at += n;
            }
            if length.is_some() {
                return Ok(body);
            }
            if self.byte().await? != b'\r' || self.byte().await? != b'\n' {
                return Err(Error::Invalid("invalid upload chunk terminator"));
            }
        }
    }
    async fn response(&mut self, r: Response) -> Result {
        let body = r.body.bytes();
        let head = format!(
            "HTTP/1.1 {} {}\r\nContent-Type: {}\r\nContent-Length: {}\r\nConnection: close\r\nCache-Control: no-store\r\nX-Content-Type-Options: nosniff\r\n\r\n",
            r.status,
            if r.status < 400 { "OK" } else { "Error" },
            r.content_type,
            body.len()
        );
        self.stream
            .set_timeout(Some(core::time::Duration::from_secs(15)));
        self.stream
            .write_all(head.as_bytes())
            .await
            .map_err(|_| Error::Invalid("upload response failed"))?;
        self.stream
            .write_all(body)
            .await
            .map_err(|_| Error::Invalid("upload response failed"))
    }
    async fn receive(&mut self) -> Result<Response> {
        let (mut request, length, expect) = self.head().await?;
        let (reply, answers) = Sender::channel()?;
        let authorization = Request {
            method: json::copy(&request.method)?,
            path: json::copy(&request.path)?,
            query: json::copy(&request.query)?,
            host: json::copy(&request.host)?,
            origin: json::copy(&request.origin)?,
            cookie: json::copy(&request.cookie)?,
            headers: json::object(),
            body: Vec::new(),
        };
        WORK.try_send(Work::AuthorizeRestore(Call {
            request: authorization,
            reply: reply.clone(),
        }))
        .map_err(|_| Error::Full)?;
        let response = answers.wait().await?;
        if response.status != 200 {
            return Ok(response);
        }
        if expect {
            self.stream
                .write_all(b"HTTP/1.1 100 Continue\r\n\r\n")
                .await
                .map_err(|_| Error::Invalid("upload continue failed"))?;
        }
        self.stream
            .set_timeout(Some(core::time::Duration::from_secs(120)));
        request.body = self.body(length).await?;
        WORK.try_send(Work::Request(Call { request, reply }))
            .map_err(|_| Error::Full)?;
        answers.wait().await
    }
    pub(crate) async fn serve(mut self) -> Result {
        let response = self.receive().await.or_else(|e| {
            Response::error(
                if matches!(e, Error::Full) { 413 } else { 400 },
                &alloc::format!("{e}"),
            )
        })?;
        self.response(response).await
    }
}
