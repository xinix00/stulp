//! Eén DNS-werker gebruikt de systeemresolver, inclusief mDNSResponder voor .local op macOS.
use std::{
    net::ToSocketAddrs,
    sync::mpsc::{self, Receiver, SyncSender},
    thread::JoinHandle,
};
use stulp_sdk::{Error, Result};
pub(super) struct Worker {
    input: Option<SyncSender<String>>,
    output: Receiver<Result<Vec<String>>>,
    thread: Option<JoinHandle<()>>,
    busy: bool,
}
impl Worker {
    pub(super) fn new() -> Result<Self> {
        let (input, requests) = mpsc::sync_channel(1);
        let (answers, output) = mpsc::sync_channel(1);
        let thread = std::thread::Builder::new()
            .name("stulp-plugin-dns".into())
            .spawn(move || {
                while let Ok(address) = requests.recv() {
                    if answers.send(lookup(address)).is_err() {
                        break;
                    }
                }
            })
            .map_err(|_| Error::Transport("cannot start resolver"))?;
        Ok(Self {
            input: Some(input),
            output,
            thread: Some(thread),
            busy: false,
        })
    }
    pub(super) fn start(&mut self, address: String) -> Result {
        if address.is_empty() || address.len() > 320 {
            return Err(Error::Invalid("invalid resolver address"));
        }
        if self.busy {
            if self.output.try_recv().is_err() {
                return Err(Error::Transport("previous DNS query is still ending"));
            }
            self.busy = false;
        }
        self.input
            .as_ref()
            .ok_or(Error::Transport("resolver stopped"))?
            .try_send(address)
            .map_err(|_| Error::Transport("resolver busy"))?;
        self.busy = true;
        Ok(())
    }
    pub(super) fn poll(&mut self) -> Option<Result<Vec<String>>> {
        match self.output.try_recv() {
            Ok(r) => {
                self.busy = false;
                Some(r)
            }
            Err(mpsc::TryRecvError::Empty) => None,
            Err(_) => Some(Err(Error::Transport("resolver stopped"))),
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
fn lookup(address: String) -> Result<Vec<String>> {
    let (host, port) = address
        .rsplit_once(':')
        .ok_or(Error::Invalid("resolver requires host:port"))?;
    let port = port
        .parse::<u16>()
        .ok()
        .filter(|p| *p > 0)
        .ok_or(Error::Invalid("invalid resolver port"))?;
    let host = host
        .strip_prefix('[')
        .and_then(|s| s.strip_suffix(']'))
        .unwrap_or(host);
    let mut out = Vec::new();
    for address in (host, port)
        .to_socket_addrs()
        .map_err(|_| Error::Transport("name resolution failed"))?
    {
        let address = address.to_string();
        if !out.contains(&address) {
            stulp_core::json::push(&mut out, address, 32)?;
        }
    }
    if out.is_empty() {
        return Err(Error::Transport("name has no usable addresses"));
    }
    Ok(out)
}
#[cfg(test)]
#[allow(clippy::unwrap_used)]
mod tests {
    use super::*;
    #[test]
    fn system_resolves_loopback_without_lan_traffic() {
        for name in ["localhost:5540", "[::1]:5540", "127.0.0.1:5540"] {
            for address in lookup(name.into()).unwrap() {
                let address: std::net::SocketAddr = address.parse().unwrap();
                assert!(address.ip().is_loopback());
                assert_eq!(address.port(), 5540);
            }
        }
    }
}
