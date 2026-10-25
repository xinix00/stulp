//! Eén HTTP-verzoek van een plugin, blokkerend op de HTTP-werker: naar de
//! cloud met keten- en naamcontrole (`WebDial`), of naar een lokaal apparaat
//! met een eigen certificaat. Alleen expliciete verzoeken
//! (`device_certificate`) nemen het apparaatpad; dat volgt geen redirects.
use std::time::{Duration, Instant};
use stulp_core::json;
use stulp_sdk::{Error, HttpRequest, HttpResponse, Result};
use stulp_transport::web::{self, Blocking, Tcp};

/// De meldingen van één pad; operators en tests kennen ze.
struct Texts {
    header: &'static str,
    send: &'static str,
    body: &'static str,
}

const CLOUD: Texts = Texts {
    header: "HTTP connection, TLS or response failed",
    send: "HTTP connection, TLS or response failed",
    body: "HTTP connection, TLS or response failed",
};

const DEVICE: Texts = Texts {
    header: "invalid device HTTP header",
    send: "device TLS 1.3 connection or HTTP failed",
    body: "device HTTP body failed",
};

pub(super) fn execute(request: HttpRequest) -> Result<HttpResponse> {
    // Als HTTP/1.1 versturen zou bij een HTTP/2-dienst (APNs) iets onleesbaars
    // opleveren; liever een duidelijke fout tot de HTTP/2-client er is.
    if request.http2 {
        return Err(Error::Invalid(
            "HTTP/2 is op deze host nog niet beschikbaar",
        ));
    }
    let timeout = Duration::from_millis(request.timeout_ms);
    // Eén budget voor verbinden, TLS, kop, body en elke redirect; de
    // termijn geldt ook per fase en als stilte per lees of schrijf.
    let tcp = Tcp::new(
        Blocking,
        timeout,
        Some(timeout),
        Some(Instant::now() + timeout),
    );
    stulp_platform::host::block_on(async {
        if request.device_certificate {
            send(web::device(tcp), &request, timeout, &DEVICE).await
        } else {
            send(web::web(tcp), &request, timeout, &CLOUD).await
        }
    })
}

async fn send<D: leanhttp::Dial>(
    mut dial: D,
    request: &HttpRequest,
    timeout: Duration,
    texts: &Texts,
) -> Result<HttpResponse> {
    let mut header = leanhttp::Header::new();
    for (k, v) in &request.headers {
        header.set(k, v).map_err(|_| Error::Invalid(texts.header))?;
    }
    let call = leanhttp::Call {
        method: &request.method,
        url: &request.url,
        header,
        body: Some(&request.body),
        header_timeout: Some(timeout),
        no_follow: request.device_certificate,
        ..Default::default()
    };
    let mut reply = leanhttp::fetch(&mut dial, call)
        .await
        .map_err(|_| Error::Transport(texts.send))?;
    let mut headers = Vec::new();
    for (k, v) in reply.header.iter() {
        json::push(&mut headers, (json::copy(k)?, json::copy(v)?), 256)?;
    }
    for cookie in &reply.set_cookie {
        json::push(
            &mut headers,
            (json::copy("Set-Cookie")?, json::copy(cookie)?),
            256,
        )?;
    }
    let body = reply
        .read_to_end(request.limit)
        .await
        .map_err(|_| Error::Transport(texts.body))?;
    // Geen pool: de verbinding gaat dicht (Drop), elk verzoek dialt vers.
    Ok(HttpResponse {
        status: reply.status,
        headers,
        body,
    })
}
