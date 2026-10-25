//! Eén plugin-HTTP-verzoek over HTTP/2, voor host en HopOS.
//!
//! Alleen voor diensten die niets anders spreken; nu is dat Apple's pushdienst
//! APNs (`HttpRequest::http2`). De aanroeper geeft een kale TCP-dialer met zijn
//! eigen termijnen, de wandklok en verse entropie; deze crate doet TLS met ALPN
//! `h2` tegen de Mozilla-wortels en stuurt het verzoek met
//! `leanh2::client::send`. Eén verzoek per verbinding: APNs ziet er een paar
//! per dag, en dan is een open verbinding bijhouden meer staat dan het waard is.
//!
//! TLS en HTTP/2 komen uit Lean v3.1.15 (ALPN, de HTTP/2-client en het lege
//! clientcertificaat dat APNs vraagt); de TCP-kant blijft op de leanhttp van
//! HopOS' `applib`. Neemt Stulp een `applib` op dezelfde Lean, dan kan dit
//! weer over `leanhttps::WebDial` met `alpn`.
#![no_std]
#![forbid(unsafe_code)]
extern crate alloc;

use alloc::{string::String, vec::Vec};
use core::future::poll_fn;
use core::pin::Pin;
use core::task::{Context, Poll, ready};
use leanhttp::IoError;
use leantls::{ChainVerifier, ConnError, Entropy, Options, Roots, Trust};
use stulp_core::json;
use stulp_sdk::{Error, HttpRequest, HttpResponse, Result};

/// Bytes verse entropie per handshake.
pub const ENTROPY: usize = Entropy::LEN;

/// Een kale leanhttp-verbinding als transport onder leantls.
///
/// leanhttp scheidt schrijven en doorduwen, leantls kent alleen schrijven.
/// Daarom wordt elk geschreven stuk meteen doorgeduwd, en meldt `poll_write`
/// de bytes pas als dat gelukt is: anders kan een laatste record in een buffer
/// blijven hangen terwijl beide kanten op elkaar wachten.
struct Raw<'a, C> {
    conn: &'a mut C,
    flushing: Option<usize>,
}

impl<C: leanhttp::Conn + Unpin> leantls::AsyncRead for Raw<'_, C> {
    type Error = IoError;
    fn poll_read(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut [u8],
    ) -> Poll<core::result::Result<usize, IoError>> {
        self.get_mut().conn.poll_read(cx, buf)
    }
}

impl<C: leanhttp::Conn + Unpin> leantls::AsyncWrite for Raw<'_, C> {
    type Error = IoError;
    fn poll_write(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<core::result::Result<usize, IoError>> {
        let this = self.get_mut();
        loop {
            // Na een Pending komt dezelfde aanroep terug; die bytes stonden al
            // in de buffer, dus nu alleen het doorduwen afmaken.
            if let Some(written) = this.flushing {
                ready!(this.conn.poll_flush(cx))?;
                this.flushing = None;
                return Poll::Ready(Ok(written));
            }
            let written = ready!(this.conn.poll_write(cx, buf))?;
            this.flushing = Some(written);
        }
    }
}

/// De TLS-sessie als leanh2-transport; leantls duwt elk record zelf door.
struct Tls<'a, 'b, C>(&'a mut leantls::Conn<Raw<'b, C>>);

impl<C: leanhttp::Conn + Unpin> leanh2::AsyncRead for Tls<'_, '_, C> {
    type Error = ConnError<IoError>;
    fn poll_read(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut [u8],
    ) -> Poll<core::result::Result<usize, Self::Error>> {
        leantls::AsyncRead::poll_read(Pin::new(&mut *self.get_mut().0), cx, buf)
    }
}

impl<C: leanhttp::Conn + Unpin> leanh2::AsyncWrite for Tls<'_, '_, C> {
    type Error = ConnError<IoError>;
    fn poll_write(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<core::result::Result<usize, Self::Error>> {
        leantls::AsyncWrite::poll_write(Pin::new(&mut *self.get_mut().0), cx, buf)
    }
    fn poll_close(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<core::result::Result<(), Self::Error>> {
        self.get_mut().0.poll_close_notify(cx)
    }
}

/// `https://host[:poort]/pad` in host, poort en pad; alleen https.
fn split(url: &str) -> Result<(&str, u16, &str)> {
    let rest = url
        .strip_prefix("https://")
        .ok_or(Error::Invalid("HTTP/2 alleen over https"))?;
    let (authority, path) = match rest.find('/') {
        Some(at) => (&rest[..at], &rest[at..]),
        None => (rest, "/"),
    };
    let (host, port) = match authority.rsplit_once(':') {
        Some((host, port)) if !host.ends_with(']') || authority.starts_with('[') => (
            host,
            port.parse::<u16>()
                .map_err(|_| Error::Invalid("ongeldige poort"))?,
        ),
        _ => (authority, 443),
    };
    if host.is_empty() || host.contains(['@', ' ', '[']) {
        return Err(Error::Invalid("ongeldige host"));
    }
    Ok((host, port, path))
}

/// Stuurt `request` over een verse TCP-verbinding van `tcp`, met TLS tegen de
/// Mozilla-wortels op `now_unix` en ALPN `h2`. `seed` wordt na gebruik gewist.
pub async fn send<D>(
    tcp: &mut D,
    now_unix: Option<u64>,
    seed: &mut [u8; ENTROPY],
    request: &HttpRequest,
) -> Result<HttpResponse>
where
    D: leanhttp::Dial,
    D::Conn: Unpin,
{
    let entropy = Entropy::new(*seed);
    seed.fill(0);
    let (host, port, path) = split(&request.url)?;
    let now = now_unix.ok_or(Error::Transport("HTTP/2: geen wandklok voor de keten"))?;
    let roots = Roots::from_concatenated_der(leantls::MOZILLA_ROOTS)
        .map_err(|_| Error::Transport("HTTP/2: wortelcertificaten onleesbaar"))?;
    let verifier = ChainVerifier::new(roots, now);
    let mut conn = tcp
        .dial(leanhttp::Target {
            https: false,
            host,
            port,
        })
        .await
        .map_err(|_| Error::Transport("HTTP/2: verbinden mislukt"))?;
    let raw = Raw {
        conn: &mut conn,
        flushing: None,
    };
    let options = Options::new().alpn(&[b"h2"]);
    let mut tls = leantls::connect_with(raw, &Trust::Chain(&verifier), host, entropy, &options)
        .await
        .map_err(|_| Error::Transport("HTTP/2: TLS mislukt"))?;
    if tls.alpn() != Some(b"h2".as_slice()) {
        return Err(Error::Transport("HTTP/2: de server koos geen h2"));
    }
    let mut header: Vec<(&str, &str)> = Vec::new();
    header
        .try_reserve_exact(request.headers.len())
        .map_err(|_| stulp_core::Error::Memory)?;
    for (k, v) in &request.headers {
        header.push((k.as_str(), v.as_str()));
    }
    let authority = if port == 443 {
        String::from(host)
    } else {
        stulp_sdk::util::join(&[host, ":", &alloc::format!("{port}")])?
    };
    let h2 = leanh2::client::Request {
        method: &request.method,
        authority: &authority,
        path,
        header: &header,
        body: &request.body,
    };
    let limits = leanh2::client::Limits {
        header_list: 16 << 10,
        body: request.limit,
    };
    let response = leanh2::client::send(&mut Tls(&mut tls), &h2, &limits)
        .await
        .map_err(|e| match e {
            leanh2::client::SendError::BodyTooLarge { .. } => {
                Error::Invalid("HTTP/2: antwoord groter dan de grens")
            }
            leanh2::client::SendError::Request(why) => Error::Invalid(why),
            _ => Error::Transport("HTTP/2: verzoek of antwoord mislukt"),
        })?;
    // Best effort: het antwoord is binnen, een trage afsluiting telt niet.
    let _ = poll_fn(|cx| tls.poll_close_notify(cx)).await;
    drop(tls);
    let _ = leanhttp::close(&mut conn).await;
    let mut headers = Vec::new();
    for (k, v) in &response.header {
        json::push(&mut headers, (json::copy(k)?, json::copy(v)?), 256)?;
    }
    Ok(HttpResponse {
        status: response.status,
        headers,
        body: response.body,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn urls_split_into_host_port_and_path() -> Result {
        assert_eq!(
            split("https://api.push.apple.com/3/device/ab")?,
            ("api.push.apple.com", 443, "/3/device/ab")
        );
        assert_eq!(split("https://h:8443")?, ("h", 8443, "/"));
        assert!(split("http://api.push.apple.com/").is_err());
        assert!(split("https://user@h/").is_err());
        assert!(split("https://h:x/").is_err());
        Ok(())
    }
}
