//! De controller decodeert niets: één bestaande HTTP-werker streamt een private bron door.
use std::time::Duration;
use stulp_core::{
    Error, Result,
    json::{self, Value},
};
use stulp_web::{Body, Response};
pub(crate) fn response(value: &Value, owner: u64, image: bool) -> Result<Response> {
    let url = json::text(value, "url");
    let mime = json::text(value, "contentType");
    if !(url.starts_with("http://") || url.starts_with("https://"))
        || url.len() > 4096
        || url.bytes().any(|b| b <= 32 || b == 127)
    {
        return Err(Error::Invalid("app supplied an invalid HTTP media address"));
    }
    if (image && !mime.starts_with("image/"))
        || !(mime.starts_with("video/") || mime.starts_with("image/"))
        || mime.len() > 128
        || mime.bytes().any(|b| b < 32 || b == 127)
    {
        return Err(Error::Invalid("app supplied an invalid media content type"));
    }
    Ok(Response {
        status: 200,
        content_type: "application/octet-stream",
        body: Body::Proxy {
            url: json::copy(url)?,
            mime: json::copy(mime)?,
            owner,
        },
        cookie: None,
        headers: Vec::new(),
    })
}
pub(crate) async fn pipe(
    exchange: &mut leanhttp::Exchange<'_, stulp_transport::Http<'_>>,
    url: &str,
    mime: &str,
) -> leanhttp::Result<()> {
    let client = hostnet::Http::new();
    let mut upstream = match client.open(&hostnet::Call::get(url, Duration::from_secs(30))) {
        Ok(source) if (200..300).contains(&source.status()) => source,
        _ => return exchange.error(502, "app media source unavailable").await,
    };
    if mime.starts_with("image/")
        && let Some(length) = upstream.header("Content-Length")
        && length.parse::<usize>().ok().is_none_or(|n| n > 4 << 20)
    {
        return exchange.error(502, "app image exceeds 4 MiB").await;
    }
    let header = exchange.header_mut();
    header.set("Content-Type", mime)?;
    header.set("Cache-Control", "no-store")?;
    header.set("X-Content-Type-Options", "nosniff")?;
    header.set("Referrer-Policy", "no-referrer")?;
    exchange.write_header(200)?;
    let mut buffer = [0; 8192];
    let mut total = 0usize;
    loop {
        let n = upstream
            .read(&mut buffer)
            .map_err(|_| leanhttp::Error::Connect)?;
        if n == 0 {
            return Ok(());
        }
        if mime.starts_with("image/") {
            total = total.saturating_add(n);
            if total > 4 << 20 {
                return Err(leanhttp::Error::Connect);
            }
        }
        exchange.write(&buffer[..n]).await?;
        exchange.flush().await?;
    }
}
