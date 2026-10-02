// Synthetische apparaten en HTTP-antwoorden, met de echte controllerlogica ertussen.
#![allow(dead_code)]
use std::{collections::VecDeque, format, vec, vec::Vec};
use stulp_core::{
    json::{self, Value},
    store::{Memory, Store},
};
use stulp_protocol::{Frame, Kind};
use stulp_runtime::App;
use stulp_sdk::{Client, Error, Event, HttpRequest, HttpResponse, Result, Transport};
pub(crate) struct Wire {
    pub(crate) store: Store<Memory>,
    app: App,
    inbox: VecDeque<Frame>,
    pub(crate) sent: Vec<Value>,
    pub(crate) http: Vec<HttpRequest>,
    pub(crate) tcp: Vec<stulp_sdk::TcpRequest>,
    pub(crate) tcp_bodies: VecDeque<Vec<u8>>,
    tcp_answer: Option<Result<Vec<u8>>>,
    pub(crate) tcp_delay: u64,
    tcp_ready: u64,
    pub(crate) replies: VecDeque<HttpResponse>,
    pub(crate) delay: u64,
    pub(crate) fail_store: bool,
    pub(crate) fail_state: bool,
    ready: Option<u64>,
    now: u64,
}
pub(crate) fn value(s: &str) -> Value {
    json::parse(s.as_bytes()).unwrap()
}
impl Wire {
    pub(crate) fn reconnect(mut self, id: &str, manifest: &[u8]) -> Client<Self> {
        self.app = App::new(id, json::parse(manifest).unwrap()).unwrap();
        self.inbox.clear();
        self.now = 0;
        let mut client = Client::new(self);
        hostnet::block_on(client.hello(id)).unwrap();
        client
    }

    pub(crate) fn client(id: &str, manifest: &[u8], devices: &str, state: &str) -> Client<Self> {
        let doc = format!(
            r#"{{"version":2,"apps":[{{"id":"{id}","enabled":true}}],"devices":{devices},"appState":{{"{id}":{state}}}}}"#
        );
        let wire = Self {
            store: Store::open(doc.as_bytes(), Memory).unwrap(),
            app: App::new(id, json::parse(manifest).unwrap()).unwrap(),
            inbox: VecDeque::new(),
            sent: Vec::new(),
            http: Vec::new(),
            tcp: Vec::new(),
            tcp_bodies: VecDeque::new(),
            tcp_answer: None,
            tcp_delay: 0,
            tcp_ready: 0,
            replies: VecDeque::new(),
            delay: 0,
            ready: None,
            now: 0,
            fail_store: false,
            fail_state: false,
        };
        let mut c = Client::new(wire);
        hostnet::block_on(c.hello(id)).unwrap();
        c
    }
    pub(crate) fn reply(&mut self, status: u16, body: &str, cookie: bool) {
        self.replies.push_back(HttpResponse {
            status,
            body: body.as_bytes().to_vec(),
            headers: if cookie {
                vec![(
                    "Set-Cookie".into(),
                    "JSESSIONID=session; Path=/; Secure; Domain=tahomalink.com".into(),
                )]
            } else {
                vec![]
            },
        });
    }
}
impl Transport for Wire {
    fn start_tcp(&mut self, request: stulp_sdk::TcpRequest) -> Result {
        let answer = self
            .tcp_bodies
            .pop_front()
            .map(|body| {
                let mut answer = request.frame[..6].to_vec();
                answer[4..6].copy_from_slice(&(body.len() as u16).to_be_bytes());
                answer.extend_from_slice(&body);
                answer
            })
            .ok_or(Error::Transport("missing test TCP response"));
        self.tcp.push(request);
        self.tcp_answer = Some(answer);
        self.tcp_ready = self.now.saturating_add(self.tcp_delay);
        Ok(())
    }
    fn poll_tcp(&mut self) -> Option<Result<Vec<u8>>> {
        if self.now < self.tcp_ready {
            return None;
        }
        self.tcp_answer.take()
    }

    async fn send(&mut self, v: &Value) -> Result {
        let f = Frame::decode(json::to_string(v).unwrap().as_bytes()).unwrap();
        self.sent.push(stulp_sdk::clone(v)?);
        if f.kind != Kind::Request {
            return Ok(());
        }
        let p = json::get(v, "p").unwrap_or(&Value::Null);
        if (self.fail_store && f.method() == "device.merge" && json::text(p, "field") == "store")
            || (self.fail_state && f.method() == "state.set")
        {
            self.inbox.push_back(
                Frame::decode(
                    json::to_string(&Frame::response(f.id, Err("disk full")).unwrap())
                        .unwrap()
                        .as_bytes(),
                )
                .unwrap(),
            );
            return Ok(());
        }
        let actions = self
            .app
            .receive(&mut self.store, f, self.now, "2026-10-01T12:00:00Z", "")
            .unwrap();
        for out in actions.outgoing {
            self.inbox
                .push_back(Frame::decode(json::to_string(&out).unwrap().as_bytes()).unwrap());
        }
        Ok(())
    }
    async fn next(&mut self) -> Result<Event> {
        self.now += 100;
        Ok(self
            .inbox
            .pop_front()
            .map(Event::Frame)
            .unwrap_or(Event::Tick))
    }
    fn now(&self) -> u64 {
        self.now
    }
    fn wall_time(&self) -> Result<u64> {
        Ok(1_790_000_000 + self.now / 1000)
    }
    fn random(&mut self) -> Result<[u8; 32]> {
        Ok([7; 32])
    }
    fn start_http(&mut self, r: HttpRequest) -> Result {
        if self.ready.is_some() {
            return Err(Error::Transport("HTTP still busy"));
        }
        self.http.push(r);
        self.ready = Some(self.now + self.delay);
        Ok(())
    }
    fn poll_http(&mut self) -> Option<Result<HttpResponse>> {
        if self.ready.is_none_or(|t| self.now < t) {
            return None;
        }
        self.ready = None;
        Some(
            self.replies
                .pop_front()
                .ok_or(Error::Transport("missing test HTTP response")),
        )
    }
}
