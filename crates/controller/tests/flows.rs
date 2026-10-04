//! Een Flow met meerdere acties: een mislukte of onverzendbare actie houdt de rest niet tegen.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
use std::{cell::RefCell, rc::Rc};
use stulp_controller::{Apps, Inbox, Reply, flows::Flows, scenes::Scenes};
use stulp_core::{
    Result,
    json::{self, Value},
    store::{Memory, Store},
};
use stulp_runtime::Completion;
use stulp_web::{Body, Environment, Response};

#[derive(Clone, Default)]
struct Route(Rc<RefCell<Option<Response>>>);
impl Inbox for Route {
    fn receive(&self) -> Result<Option<Response>> {
        Ok(self.0.borrow_mut().take())
    }
}
impl Reply for Route {
    type Inbox = Route;
    fn channel() -> Result<(Self, Self::Inbox)> {
        let route = Route::default();
        Ok((route.clone(), route))
    }
    fn send(&self, response: Response) -> Result {
        *self.0.borrow_mut() = Some(response);
        Ok(())
    }
}

#[derive(Default)]
struct FakeApps {
    now: u64,
    calls: Vec<(String, u64, String, Value)>,
    logs: RefCell<Vec<String>>,
}
impl Apps for FakeApps {
    fn now(&self) -> u64 {
        self.now
    }
    fn running(&self, _: &str) -> bool {
        true
    }
    fn call(&mut self, app: &str, owner: u64, method: &str, params: &Value) -> Result {
        if app == "com.stulp.absent" {
            return Err(stulp_core::Error::Missing("app is not connected"));
        }
        self.calls.push((
            app.to_string(),
            owner,
            method.to_string(),
            json::TryClone::try_clone(params)?,
        ));
        Ok(())
    }
    fn adopt(&mut self, _: &Value) -> Result {
        Ok(())
    }
    fn log(&self, line: core::fmt::Arguments<'_>) {
        self.logs.borrow_mut().push(line.to_string());
    }
}

struct Env;
impl Environment for Env {
    fn id(&mut self) -> Result<String> {
        Ok("id".into())
    }
    fn now(&self) -> Result<String> {
        Ok("2026-10-04T08:00:00Z".into())
    }
}

#[test]
fn welcome_flow_runs_every_action_even_when_spotify_times_out() {
    let mut store = Store::open(
        br#"{"version":2,
            "apps":[{"id":"com.stulp.spotify","enabled":true},{"id":"com.stulp.matter","enabled":true}],
            "devices":[{"id":"lamp","appId":"com.stulp.matter","driverId":"light","capabilities":["onoff"]}],
            "flows":[{"id":"welkom","name":"Welkom op kantoor","enabled":true,
              "nodes":[
                {"id":"t","step":{"appId":"stulp","cardType":"trigger","cardId":"time_at"}},
                {"id":"spotify","step":{"appId":"com.stulp.spotify","cardType":"action","cardId":"play_playlist"}},
                {"id":"absent","step":{"appId":"com.stulp.absent","cardType":"action","cardId":"say"}},
                {"id":"lamp","step":{"appId":"stulp","cardType":"action","cardId":"capability.onoff.turn_on","args":{"device":{"$device":"lamp"}}}}],
              "edges":[{"id":"1","from":"t","to":"spotify"},{"id":"2","from":"t","to":"absent"},{"id":"3","from":"t","to":"lamp"}]}]}"#,
        Memory,
    )
    .unwrap();
    let mut flows = Flows::<Route>::default();
    let mut scenes = Scenes::<Route>::new();
    let mut apps = FakeApps::default();
    let mut env = Env;
    let mut owner = 100;
    let (reply, inbox) = Route::channel().unwrap();
    flows.start(&store, "welkom", 7, 0, reply, &env).unwrap();

    apps.now = 1;
    flows.poll(&mut store, &mut apps, &mut env, &mut owner, &mut scenes);
    assert_eq!(apps.calls.len(), 1);
    assert_eq!(apps.calls[0].0, "com.stulp.spotify");
    assert_eq!(json::text(&apps.calls[0].3, "id"), "play_playlist");
    // De Spotify-callback verloopt: de controller levert een mislukte completion.
    let timed_out = Completion {
        owner: 7,
        failed: true,
        value: json::parse(br#"{"message":"app callback timed out"}"#).unwrap(),
    };
    assert!(flows.complete(&timed_out, 30_001).unwrap());

    // De volgende actie hoort bij een app die niet verbonden is; ook dat stopt de rest niet.
    for now in 30_002..30_010 {
        apps.now = now;
        flows.poll(&mut store, &mut apps, &mut env, &mut owner, &mut scenes);
        if apps.calls.len() > 1 {
            break;
        }
    }
    assert_eq!(apps.calls.len(), 2, "the lamp action never ran");
    assert_eq!(
        (apps.calls[1].0.as_str(), apps.calls[1].2.as_str()),
        ("com.stulp.matter", "capability.invoke")
    );
    assert_eq!(json::text(&apps.calls[1].3, "deviceId"), "lamp");
    let done = Completion {
        owner: 7,
        failed: false,
        value: Value::Null,
    };
    assert!(flows.complete(&done, 30_020).unwrap());
    apps.now = 30_021;
    flows.poll(&mut store, &mut apps, &mut env, &mut owner, &mut scenes);
    let response = inbox.receive().unwrap().expect("flow finished");
    assert_eq!(response.status, 502);
    let Body::Owned(body) = response.body else {
        panic!("unexpected body");
    };
    assert!(body.contains("app callback timed out"), "{body}");
    let record = store.document().record("flows", "welkom").unwrap();
    assert_eq!(json::text(record, "lastError"), "app callback timed out");
}

#[test]
fn a_triggered_run_logs_its_queue_wait_and_card_times() {
    // Vier runs wachten op een trage app; de beweging moet in de rij wachten
    // tot er plek is, en de logregel zegt hoe lang, en hoe lang de lamp duurde.
    let mut store = Store::open(
        br#"{"version":2,
            "apps":[{"id":"com.stulp.spotify","enabled":true},{"id":"com.stulp.matter","enabled":true}],
            "devices":[{"id":"lamp","appId":"com.stulp.matter","driverId":"light","capabilities":["onoff"]}],
            "flows":[
              {"id":"wacht","name":"Wacht","enabled":true,
               "nodes":[{"id":"t","step":{"appId":"stulp","cardType":"trigger","cardId":"time_at"}},
                        {"id":"s","step":{"appId":"com.stulp.spotify","cardType":"action","cardId":"play_playlist"}}],
               "edges":[{"id":"1","from":"t","to":"s"}]},
              {"id":"gang","name":"Beweging gang","enabled":true,
               "nodes":[{"id":"t","step":{"appId":"com.stulp.matter","cardType":"trigger","cardId":"motion"}},
                        {"id":"lamp","step":{"appId":"stulp","cardType":"action","cardId":"capability.onoff.turn_on","args":{"device":{"$device":"lamp"}}}}],
               "edges":[{"id":"1","from":"t","to":"lamp"}]}]}"#,
        Memory,
    )
    .unwrap();
    let mut flows = Flows::<Route>::default();
    let mut scenes = Scenes::<Route>::new();
    let mut apps = FakeApps::default();
    let mut env = Env;
    let mut owner = 100;
    for busy in 1..=4 {
        let (reply, _) = Route::channel().unwrap();
        flows.start(&store, "wacht", busy, 0, reply, &env).unwrap();
    }
    flows.poll(&mut store, &mut apps, &mut env, &mut owner, &mut scenes);
    store
        .trigger(
            "com.stulp.matter",
            &json::parse(br#"{"id":"motion","kind":"trigger","tokens":{"device":"Gang"}}"#)
                .unwrap(),
        )
        .unwrap();
    apps.now = 100;
    flows.poll(&mut store, &mut apps, &mut env, &mut owner, &mut scenes);
    let started = apps.calls.len();
    assert_eq!(
        started, 4,
        "the motion run started while four runs were busy"
    );
    // Eén trage run is klaar; de beweging komt aan de beurt.
    let done = |owner| Completion {
        owner,
        failed: false,
        value: Value::Null,
    };
    assert!(flows.complete(&done(1), 900).unwrap());
    for now in [1000, 1100] {
        apps.now = now;
        flows.poll(&mut store, &mut apps, &mut env, &mut owner, &mut scenes);
    }
    // Eerst vraagt Stulp de app of de trigger past (het triggerfilter).
    let filter = apps.calls.last().unwrap();
    assert_eq!(
        (filter.0.as_str(), filter.2.as_str()),
        ("com.stulp.matter", "flow.run")
    );
    let filter_owner = filter.1;
    let passed = Completion {
        owner: filter_owner,
        failed: false,
        value: Value::Bool(true),
    };
    assert!(flows.complete(&passed, 1150).unwrap());
    apps.now = 1200;
    flows.poll(&mut store, &mut apps, &mut env, &mut owner, &mut scenes);
    let lamp = apps.calls.last().unwrap();
    assert_eq!(
        (lamp.0.as_str(), lamp.2.as_str()),
        ("com.stulp.matter", "capability.invoke")
    );
    assert!(flows.complete(&done(lamp.1), 1450).unwrap());
    apps.now = 1460;
    flows.poll(&mut store, &mut apps, &mut env, &mut owner, &mut scenes);
    let logs = apps.logs.borrow();
    let line = logs
        .iter()
        .find(|l| l.starts_with("[stulp:flow-time]"))
        .unwrap_or_else(|| panic!("no timing line in {logs:?}"));
    assert_eq!(
        line,
        "[stulp:flow-time] flow=\"Beweging gang\" trigger=motion device=\"Gang\" lag_ms=1000 run_ms=360 cards=com.stulp.matter/motion:50,com.stulp.matter/onoff:250"
    );
}
