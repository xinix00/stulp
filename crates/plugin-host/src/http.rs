//! Eén vaste HTTP-werker per pluginverbinding; control en heartbeat blijven bij hun eigenaar.
use std::{
    sync::mpsc::{self, Receiver, SyncSender},
    thread::JoinHandle,
    time::{Duration, Instant},
};
use stulp_sdk::{Error, HttpRequest, HttpResponse, Result};
pub(super) struct Worker {
    input: Option<SyncSender<HttpRequest>>,
    output: Receiver<Result<HttpResponse>>,
    thread: Option<JoinHandle<()>>,
    busy: bool,
}
impl Worker {
    pub(super) fn new() -> Result<Self> {
        let (input, requests) = mpsc::sync_channel::<HttpRequest>(1);
        let (answers, output) = mpsc::sync_channel(1);
        let thread = std::thread::Builder::new()
            .name("stulp-plugin-http".into())
            .spawn(move || {
                let client = hostnet::Http::new();
                while let Ok(request) = requests.recv() {
                    let result = execute(&client, request);
                    if answers.send(result).is_err() {
                        break;
                    }
                }
            })
            .map_err(|_| Error::Transport("cannot start HTTP worker"))?;
        Ok(Self {
            input: Some(input),
            output,
            thread: Some(thread),
            busy: false,
        })
    }
    pub(super) fn start(&mut self, request: HttpRequest) -> Result {
        if self.busy {
            // Een verlopen eigenaar mag geen laat antwoord voor zijn opvolger houden.
            match self.output.try_recv() {
                Ok(_) => self.busy = false,
                Err(_) => return Err(Error::Transport("previous HTTP call is still ending")),
            }
        }
        if request.limit > 8 << 20
            || request.body.len() > 8 << 20
            || request.timeout_ms == 0
            || request.timeout_ms > 60_000
        {
            return Err(Error::Invalid("HTTP request exceeds adapter bounds"));
        }
        self.input
            .as_ref()
            .ok_or(Error::Transport("HTTP worker stopped"))?
            .try_send(request)
            .map_err(|_| Error::Transport("HTTP worker unavailable"))?;
        self.busy = true;
        Ok(())
    }
    pub(super) fn poll(&mut self) -> Option<Result<HttpResponse>> {
        match self.output.try_recv() {
            Ok(result) => {
                self.busy = false;
                Some(result)
            }
            Err(mpsc::TryRecvError::Empty) => None,
            Err(mpsc::TryRecvError::Disconnected) => {
                Some(Err(Error::Transport("HTTP worker stopped")))
            }
        }
    }
}
impl Drop for Worker {
    fn drop(&mut self) {
        self.input.take();
        if let Some(thread) = self.thread.take() {
            let _ = thread.join();
        }
    }
}
fn execute(client: &hostnet::Http, request: HttpRequest) -> Result<HttpResponse> {
    if request.device_certificate {
        return super::device_tls::execute(request);
    }
    let mut headers = Vec::new();
    headers
        .try_reserve(request.headers.len())
        .map_err(|_| stulp_core::Error::Memory)?;
    for (key, value) in &request.headers {
        headers.push((key.as_str(), value.as_str()));
    }
    let timeout = Duration::from_millis(request.timeout_ms);
    let call = hostnet::Call {
        method: &request.method,
        url: &request.url,
        headers: &headers,
        body: Some(&request.body),
        timeout,
    };
    let response = client
        .request_until(&call, request.limit, Instant::now() + timeout)
        .map_err(|_| Error::Transport("HTTP connection, TLS or response failed"))?;
    Ok(HttpResponse {
        status: response.status,
        headers: response.headers,
        body: response.body,
    })
}
