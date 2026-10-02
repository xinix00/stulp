//! Eén verbindingseigenaar voor lokale binaire protocollen. Na een gedeeltelijk
//! antwoord wordt de socket gesloten; schrijven wordt nooit stil herhaald.
use std::{
    io::{Read, Write},
    net::{TcpStream, ToSocketAddrs},
    sync::mpsc::{self, Receiver, SyncSender},
    thread::JoinHandle,
    time::{Duration, Instant},
};
use stulp_sdk::{Error, Result, TcpRequest};
pub(super) struct Worker {
    input: Option<SyncSender<TcpRequest>>,
    output: Receiver<Result<Vec<u8>>>,
    thread: Option<JoinHandle<()>>,
    busy: bool,
}
impl Worker {
    pub(super) fn new() -> Result<Self> {
        let (input, requests) = mpsc::sync_channel(1);
        let (answers, output) = mpsc::sync_channel(1);
        let thread = std::thread::Builder::new()
            .name("stulp-plugin-tcp".into())
            .spawn(move || {
                let mut connection = Connection::default();
                while let Ok(request) = requests.recv() {
                    let result = connection.execute(request);
                    if result.is_err() {
                        connection.socket = None;
                    }
                    if answers.send(result).is_err() {
                        break;
                    }
                }
            })
            .map_err(|_| Error::Transport("cannot start TCP worker"))?;
        Ok(Self {
            input: Some(input),
            output,
            thread: Some(thread),
            busy: false,
        })
    }
    pub(super) fn start(&mut self, r: TcpRequest) -> Result {
        if self.busy {
            if self.output.try_recv().is_ok() {
                self.busy = false;
            } else {
                return Err(Error::Transport("previous TCP call is still ending"));
            }
        }
        if r.prefix < 2
            || r.prefix > 64
            || r.length_at > r.prefix - 2
            || r.minimum > r.maximum
            || r.maximum > 4096
            || r.frame.is_empty()
            || r.frame.len() > 4096
            || r.timeout_ms == 0
            || r.timeout_ms > 60_000
        {
            return Err(Error::Invalid("TCP request exceeds adapter bounds"));
        }
        self.input
            .as_ref()
            .ok_or(Error::Transport("TCP worker stopped"))?
            .try_send(r)
            .map_err(|_| Error::Transport("TCP worker unavailable"))?;
        self.busy = true;
        Ok(())
    }
    pub(super) fn poll(&mut self) -> Option<Result<Vec<u8>>> {
        match self.output.try_recv() {
            Ok(r) => {
                self.busy = false;
                Some(r)
            }
            Err(mpsc::TryRecvError::Empty) => None,
            Err(_) => Some(Err(Error::Transport("TCP worker stopped"))),
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
#[derive(Default)]
struct Connection {
    socket: Option<TcpStream>,
    address: String,
    generation: u64,
}
fn left(at: Instant) -> Result<Duration> {
    let left = at.saturating_duration_since(Instant::now());
    if left.is_zero() {
        Err(Error::Timeout)
    } else {
        Ok(left.max(Duration::from_millis(1)))
    }
}
fn read(socket: &mut TcpStream, mut bytes: &mut [u8], at: Instant) -> Result {
    while !bytes.is_empty() {
        socket
            .set_read_timeout(Some(left(at)?))
            .map_err(|_| Error::Transport("TCP read deadline failed"))?;
        match socket.read(bytes) {
            Ok(0) => return Err(Error::Transport("TCP peer closed during frame")),
            Ok(n) => bytes = &mut bytes[n..],
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => (),
            Err(_) => return Err(Error::Transport("TCP read failed")),
        }
    }
    Ok(())
}
impl Connection {
    fn execute(&mut self, r: TcpRequest) -> Result<Vec<u8>> {
        let at = Instant::now() + Duration::from_millis(r.timeout_ms);
        if self.address != r.address || self.generation != r.generation {
            self.socket = None;
            self.address = r.address;
            self.generation = r.generation;
        }
        if self.socket.is_none() {
            for address in self
                .address
                .to_socket_addrs()
                .map_err(|_| Error::Transport("TCP address lookup failed"))?
                .take(16)
            {
                if let Ok(socket) = TcpStream::connect_timeout(&address, left(at)?) {
                    socket
                        .set_nodelay(true)
                        .map_err(|_| Error::Transport("TCP nodelay failed"))?;
                    self.socket = Some(socket);
                    break;
                }
            }
        }
        let socket = self
            .socket
            .as_mut()
            .ok_or(Error::Transport("TCP connect failed"))?;
        let mut bytes = r.frame.as_slice();
        while !bytes.is_empty() {
            socket
                .set_write_timeout(Some(left(at)?))
                .map_err(|_| Error::Transport("TCP write deadline failed"))?;
            match socket.write(bytes) {
                Ok(0) => return Err(Error::Transport("TCP peer closed during write")),
                Ok(n) => bytes = &bytes[n..],
                Err(e) if e.kind() == std::io::ErrorKind::Interrupted => (),
                Err(_) => return Err(Error::Transport("TCP write failed")),
            }
        }
        let mut prefix = [0; 64];
        read(socket, &mut prefix[..r.prefix], at)?;
        let length = usize::from(u16::from_be_bytes([
            prefix[r.length_at],
            prefix[r.length_at + 1],
        ]));
        if length < r.minimum || length > r.maximum {
            return Err(Error::Invalid("TCP answer exceeds protocol bounds"));
        }
        let mut answer = Vec::new();
        answer
            .try_reserve_exact(r.prefix + length)
            .map_err(|_| stulp_core::Error::Memory)?;
        answer.extend_from_slice(&prefix[..r.prefix]);
        answer.resize(r.prefix + length, 0);
        read(socket, &mut answer[r.prefix..], at)?;
        Ok(answer)
    }
}
#[cfg(test)]
mod tests {
    #![allow(clippy::unwrap_used)]
    use super::*;
    #[test]
    fn reuses_complete_frames_and_drops_untrusted_length_without_replaying() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap().to_string();
        let peer = std::thread::spawn(move || {
            let (mut first, _) = listener.accept().unwrap();
            first
                .set_read_timeout(Some(Duration::from_secs(3)))
                .unwrap();
            let mut q = [0; 5];
            for _ in 0..2 {
                first.read_exact(&mut q).unwrap();
                assert_eq!(&q, b"query");
                first.write_all(&[0, 1, 0]).unwrap();
                first.write_all(&[0, 0, 3, 247, 3, 0]).unwrap();
            }
            first.read_exact(&mut q).unwrap();
            first.write_all(&[0, 1, 0, 0, 255, 255]).unwrap();
            let (mut second, _) = listener.accept().unwrap();
            second
                .set_read_timeout(Some(Duration::from_secs(3)))
                .unwrap();
            second.read_exact(&mut q).unwrap();
            second.write_all(&[0, 1, 0, 0, 0, 3, 247, 3, 0]).unwrap();
        });
        let mut w = Worker::new().unwrap();
        for i in 0..4 {
            w.start(TcpRequest {
                address: address.clone(),
                generation: 0,
                frame: b"query".to_vec(),
                prefix: 6,
                length_at: 4,
                minimum: 2,
                maximum: 254,
                timeout_ms: 1000,
            })
            .unwrap();
            let deadline = Instant::now() + Duration::from_secs(3);
            let result = loop {
                if let Some(r) = w.poll() {
                    break r;
                }
                assert!(Instant::now() < deadline);
                std::thread::sleep(Duration::from_millis(5));
            };
            assert_eq!(result.is_ok(), i != 2);
            if let Ok(frame) = result {
                assert_eq!(frame, [0, 1, 0, 0, 0, 3, 247, 3, 0]);
            }
        }
        peer.join().unwrap();
    }
}
