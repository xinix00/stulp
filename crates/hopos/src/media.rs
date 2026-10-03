//! Media uses the same fixed HTTP worker as its browser connection.
use crate::network::Dial;
use applib::tcp::TcpConn;
use core::time::Duration;
pub(crate) use stulp_controller::media::response;
/// Opent de bron van de plugin. Die antwoordt zonder lengte en sluit daarna
/// (een livestream heeft geen einde), dus niet via `leanhttp::get`: die eist
/// een Content-Length en weigerde zo elk camerabeeld met een stille 502.
pub(crate) async fn open<D: leanhttp::Dial>(
    dial: &mut D,
    url: &str,
) -> leanhttp::Result<leanhttp::Response<D::Conn>> {
    let upstream = leanhttp::fetch(
        dial,
        leanhttp::Call {
            url,
            header_timeout: Some(Duration::from_secs(10)),
            no_follow: true,
            ..leanhttp::Call::default()
        },
    )
    .await?;
    if upstream.status != 200 {
        let status = upstream.status;
        let _ = upstream.release().await;
        return Err(leanhttp::Error::Status(status));
    }
    Ok(upstream)
}
pub(crate) async fn pipe(
    exchange: &mut leanhttp::Exchange<'_, TcpConn<crate::upload::Prefix>>,
    dial: &mut Dial,
    url: &str,
    mime: &str,
) -> leanhttp::Result<()> {
    let mut upstream = match open(dial, url).await {
        Ok(r) => r,
        Err(e) => {
            applib::log!("[stulp:media] source unavailable mime={mime} error={e}");
            return exchange.error(502, "app media source unavailable").await;
        }
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
#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::panic)]
mod tests {
    use super::open;
    use alloc::vec::Vec;
    use core::future::Future;
    use core::pin::pin;
    use core::task::{Context, Poll, Waker};
    use leanhttp::{AsyncRead, AsyncWrite, Close, IoError, Target};
    /// Het antwoord van de mediaserver van een plugin, zoals hij het schrijft.
    struct Answer(Vec<u8>, usize);
    impl AsyncRead for Answer {
        fn poll_read(
            &mut self,
            _: &mut Context<'_>,
            buf: &mut [u8],
        ) -> Poll<Result<usize, IoError>> {
            let n = buf.len().min(self.0.len() - self.1);
            buf[..n].copy_from_slice(&self.0[self.1..self.1 + n]);
            self.1 += n;
            Poll::Ready(Ok(n))
        }
    }
    impl AsyncWrite for Answer {
        fn poll_write(&mut self, _: &mut Context<'_>, buf: &[u8]) -> Poll<Result<usize, IoError>> {
            Poll::Ready(Ok(buf.len()))
        }
    }
    impl Close for Answer {
        fn poll_close(&mut self, _: &mut Context<'_>) -> Poll<Result<(), IoError>> {
            Poll::Ready(Ok(()))
        }
    }
    struct Plugin(&'static [u8]);
    impl leanhttp::Dial for Plugin {
        type Conn = Answer;
        async fn dial(&mut self, _: Target<'_>) -> leanhttp::Result<Answer> {
            Ok(Answer(self.0.to_vec(), 0))
        }
    }
    fn block_on<F: Future>(f: F) -> F::Output {
        let mut f = pin!(f);
        let mut cx = Context::from_waker(Waker::noop());
        loop {
            if let Poll::Ready(v) = f.as_mut().poll(&mut cx) {
                return v;
            }
        }
    }
    #[test]
    fn a_source_without_length_is_read_to_the_end() {
        let mut plugin = Plugin(
            b"HTTP/1.1 200 OK\r\nContent-Type: image/jpeg\r\nCache-Control: no-store\r\nConnection: close\r\n\r\n\xff\xd8jpeg\xff\xd9",
        );
        let body = block_on(async {
            let mut r = open(&mut plugin, "http://10.100.0.4:40000/token")
                .await
                .unwrap();
            let mut body = Vec::new();
            let mut buf = [0; 3];
            loop {
                let n = r.read(&mut buf).await.unwrap();
                if n == 0 {
                    return body;
                }
                body.extend_from_slice(&buf[..n]);
            }
        });
        assert_eq!(body, b"\xff\xd8jpeg\xff\xd9");
        // Zo faalde het tot 3.0.10: `get` eist een lengte die de plugin niet stuurt.
        let old = block_on(leanhttp::get(&mut plugin, "http://10.100.0.4:40000/token"));
        assert!(matches!(old, Err(leanhttp::Error::NoContentLength)));
    }
    #[test]
    fn a_refused_source_is_an_error() {
        let mut plugin = Plugin(b"HTTP/1.1 404 Not Found\r\nConnection: close\r\n\r\n");
        let result = block_on(open(&mut plugin, "http://10.100.0.4:40000/token"));
        assert!(matches!(result, Err(leanhttp::Error::Status(404))));
    }
}
