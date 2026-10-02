//! Media uses the same fixed HTTP worker as its browser connection.
use crate::network::Dial;
use applib::tcp::TcpConn;
pub(crate) use stulp_controller::media::response;
pub(crate) async fn pipe(
    exchange: &mut leanhttp::Exchange<'_, TcpConn<crate::upload::Prefix>>,
    dial: &mut Dial,
    url: &str,
    mime: &str,
) -> leanhttp::Result<()> {
    let mut upstream = match leanhttp::get(dial, url).await {
        Ok(r) if (200..300).contains(&r.status) => r,
        _ => return exchange.error(502, "app media source unavailable").await,
    };
    if mime.starts_with("image/") && upstream.length.is_some_and(|n| n > 4 << 20) {
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
        let n = upstream.read(&mut buffer).await?;
        if n == 0 {
            return Ok(());
        }
        total = total.saturating_add(n);
        if mime.starts_with("image/") && total > 4 << 20 {
            return Err(leanhttp::Error::Connect);
        }
        exchange.write(&buffer[..n]).await?;
        exchange.flush().await?;
    }
}
