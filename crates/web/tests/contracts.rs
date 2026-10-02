//! Browsercontracten zonder server, bestanden of huisapparaten.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::panic)]
use stulp_core::{
    json::{self, TryClone, Value},
    store::{Memory, Store},
};
use stulp_web::{Environment, Request, Web};
struct Env;
impl Environment for Env {
    fn id(&mut self) -> stulp_core::Result<String> {
        Ok("generated".into())
    }
    fn now(&self) -> stulp_core::Result<String> {
        Ok("2026-10-01T12:00:00Z".into())
    }
}
fn request(method: &str, path: &str, body: &str) -> Request {
    Request {
        headers: json::object(),
        query: String::new(),
        method: method.into(),
        path: path.into(),
        body: body.as_bytes().to_vec(),
        host: "localhost:8080".into(),
        origin: String::new(),
        cookie: String::new(),
    }
}
fn store() -> Store<Memory> {
    Store::open(br#"{"version":2,"apps":[{"id":"a","enabled":true}],"appState":{"a":{"secret":"private-runtime"}},"system":{"attachSecret":"private-attach"}}"#,Memory).unwrap()
}

#[test]
fn compact_views_keep_controls_and_all_live_values_without_private_metadata() {
    let mut store = store();
    for (id, class, caps, state) in [
        (
            "blind",
            "windowcoverings",
            r#"["measure_power","windowcoverings_state.2","windowcoverings_set.2"]"#,
            r#"{"windowcoverings_state.2":"idle","windowcoverings_set.2":0.3}"#,
        ),
        (
            "battery",
            "battery",
            r#"["onoff","measure_battery","measure_power"]"#,
            r#"{"onoff":true,"measure_power":42}"#,
        ),
    ] {
        let text = format!(
            r#"{{"id":"{id}","appId":"a","driverId":"d","class":"{class}","capabilities":{caps},"state":{state},"settings":{{"secret":"private-settings"}},"data":{{"id":"{id}","secret":"private-data"}}}}"#
        );
        store
            .put(
                "devices",
                json::parse(text.as_bytes()).unwrap(),
                true,
                None,
                "now",
            )
            .unwrap();
    }
    let web = Web::new("").unwrap();
    let mut req = request("GET", "/api/manager/devices/device", "");
    req.query = "view=%6fverview".into();
    let response = web.handle(&mut store, &req, &mut Env).unwrap();
    let value = json::parse(response.body.bytes()).unwrap();
    let blind = json::get(&value, "blind").unwrap();
    assert_eq!(
        json::text(blind, "quickCapability"),
        "windowcoverings_state.2"
    );
    assert_eq!(json::array(blind, "capabilities").len(), 2);
    assert_eq!(
        json::text(json::get(&value, "battery").unwrap(), "quickCapability"),
        "measure_power"
    );
    assert!(!json::boolean(blind, "detailComplete"));
    assert!(!json::boolean(blind, "capabilitiesComplete"));
    assert!(
        !std::str::from_utf8(response.body.bytes())
            .unwrap()
            .contains("private-")
    );
    req.query = "view=automation".into();
    let response = web.handle(&mut store, &req, &mut Env).unwrap();
    let value = json::parse(response.body.bytes()).unwrap();
    let blind = json::get(&value, "blind").unwrap();
    assert_eq!(json::array(blind, "capabilities").len(), 3);
    assert!(json::boolean(blind, "capabilitiesComplete"));
    assert!(
        !std::str::from_utf8(response.body.bytes())
            .unwrap()
            .contains("private-")
    );
    let cursor = store.sequence();
    store
        .observe(
            "a",
            "battery",
            json::parse(br#"{"onoff":false,"measure_power":43}"#).unwrap(),
            true,
            "",
        )
        .unwrap();
    let events = stulp_web::events_view(&store, cursor, "devices", true).unwrap();
    assert!(events.contains("capabilityValues"));
    assert!(events.contains("\"onoff\":false"));
    assert!(!events.contains("private-"));
    assert!(
        stulp_web::events_view(&store, cursor, "apps", true)
            .unwrap()
            .is_empty()
    );
    req.query = "view=bogus".into();
    assert_eq!(web.handle(&mut store, &req, &mut Env).unwrap().status, 400);
    req.path = "/api/stulp/events".into();
    assert_eq!(web.handle(&mut store, &req, &mut Env).unwrap().status, 400);
}

#[test]
fn cookie_auth_and_origin_check_precede_mutations() {
    let mut store = store();
    let web = Web::new("secret-key").unwrap();
    let before = store.document().encode().unwrap();
    let mut write = request(
        "PUT",
        "/api/manager/apps/app/a/setting/test",
        r#"{"value":true}"#,
    );
    assert_eq!(
        web.handle(&mut store, &write, &mut Env).unwrap().status,
        401
    );
    let login = web
        .handle(&mut store, &request("GET", "/secret-key", ""), &mut Env)
        .unwrap();
    write.cookie = login.cookie.unwrap().split(';').next().unwrap().into();
    write.origin = "http://evil.example".into();
    assert_eq!(
        web.handle(&mut store, &write, &mut Env).unwrap().status,
        403
    );
    assert_eq!(store.document().encode().unwrap(), before);
    write.origin = "http://localhost:8080".into();
    assert_eq!(
        web.handle(&mut store, &write, &mut Env).unwrap().status,
        200
    );
}

#[test]
fn metadata_and_system_do_not_expose_private_state() {
    let mut store = store();
    store.announce("a",json::parse(br#"{"id":"a","sdk":3,"version":"1.0","name":{"nl":"Lamp-app","en":"Lights"},"drivers":[{"id":"lamp","name":{"nl":"Lamp"},"capabilities":["onoff"],"pair":[]}]}"#).unwrap()).unwrap();
    let web = Web::new("").unwrap();
    for path in [
        "/api/manager/apps/app",
        "/api/manager/apps/app/a",
        "/api/manager/apps/app/a/locale",
        "/api/manager/drivers/driver",
        "/api/stulp/system",
    ] {
        let response = web
            .handle(&mut store, &request("GET", path, ""), &mut Env)
            .unwrap();
        assert_eq!(response.status, 200, "{path}");
        let text = std::str::from_utf8(response.body.bytes()).unwrap();
        assert!(!text.contains("private-runtime"));
        assert!(!text.contains("private-attach"));
    }
    let response = web
        .handle(
            &mut store,
            &request("GET", "/api/manager/drivers/driver/stulp:app:a:lamp", ""),
            &mut Env,
        )
        .unwrap();
    let driver = json::parse(response.body.bytes()).unwrap();
    assert_eq!(json::text(&driver, "ownerName"), "Lamp-app");
    assert!(!json::boolean(&driver, "ready"));
    store.set_app_status("a", "running").unwrap();
    let response = web
        .handle(
            &mut store,
            &request("GET", "/api/manager/drivers/driver/stulp:app:a:lamp", ""),
            &mut Env,
        )
        .unwrap();
    assert!(json::boolean(
        &json::parse(response.body.bytes()).unwrap(),
        "ready"
    ));
}

#[test]
fn null_setting_is_distinct_from_absent_and_disable_is_persistent() {
    let mut store = store();
    let web = Web::new("").unwrap();
    let path = "/api/manager/apps/app/a/setting/test";
    assert_eq!(
        web.handle(&mut store, &request("PUT", path, "{}"), &mut Env)
            .unwrap()
            .status,
        400
    );
    assert_eq!(
        web.handle(
            &mut store,
            &request("PUT", path, r#"{"value":null}"#),
            &mut Env
        )
        .unwrap()
        .status,
        200
    );
    let response = web
        .handle(&mut store, &request("GET", path, ""), &mut Env)
        .unwrap();
    assert_eq!(response.status, 200);
    assert_eq!(json::parse(response.body.bytes()).unwrap(), Value::Null);
    assert_eq!(
        web.handle(&mut store, &request("DELETE", path, ""), &mut Env)
            .unwrap()
            .status,
        200
    );
    assert_eq!(
        web.handle(&mut store, &request("GET", path, ""), &mut Env)
            .unwrap()
            .status,
        404
    );
    assert_eq!(
        web.handle(
            &mut store,
            &request("PUT", "/api/manager/apps/app/a/disable", ""),
            &mut Env
        )
        .unwrap()
        .status,
        200
    );
    let restarted = Store::open(store.document().encode().unwrap().as_bytes(), Memory).unwrap();
    assert!(!json::boolean(
        restarted.document().record("apps", "a").unwrap(),
        "enabled"
    ));
}

#[test]
fn management_routes_preserve_metadata_and_capability_value_contract() {
    let mut s = store();
    s.announce(
        "a",
        json::parse(br#"{"id":"a","version":"1","sdk":3,"name":"Original app"}"#).unwrap(),
    )
    .unwrap();
    s.put("devices",json::parse(br#"{"id":"d","appId":"a","driverId":"lamp","name":"Lamp","data":{},"capabilities":["onoff"],"store":{"manufacturer":"Maker","credential":"private-device"}}"#).unwrap(),true,None,"now").unwrap();
    s.observe(
        "a",
        "d",
        json::parse(br#"{"onoff":true}"#).unwrap(),
        true,
        "",
    )
    .unwrap();
    let web = Web::new("").unwrap();
    let answer = web
        .handle(
            &mut s,
            &request("GET", "/api/manager/devices/device/d/capability/onoff", ""),
            &mut Env,
        )
        .unwrap();
    assert_eq!(answer.body.bytes(), b"true");
    let answer = web
        .handle(
            &mut s,
            &request(
                "PUT",
                "/api/manager/devices/device/d",
                r#"{"name":"New name","hidden":true,"note":"A note","iconOverride":"lamp"}"#,
            ),
            &mut Env,
        )
        .unwrap();
    assert_eq!(answer.status, 200);
    let d = json::parse(answer.body.bytes()).unwrap();
    assert!(json::boolean(&d, "hidden"));
    assert_eq!(json::text(&d, "note"), "A note");
    assert_eq!(json::text(&d, "manufacturer"), "Maker");
    assert_eq!(json::text(&d, "hardwareName"), "Lamp");
    assert!(
        !std::str::from_utf8(answer.body.bytes())
            .unwrap()
            .contains("private-device")
    );
    let answer = web
        .handle(
            &mut s,
            &request("POST", "/api/stulp/device-groups", r#"{"name":"Room"}"#),
            &mut Env,
        )
        .unwrap();
    assert_eq!(answer.status, 201);
    assert_eq!(
        web.handle(
            &mut s,
            &request(
                "PUT",
                "/api/stulp/devices/d/group",
                r#"{"groupId":"generated"}"#
            ),
            &mut Env
        )
        .unwrap()
        .status,
        200
    );
    assert_eq!(
        web.handle(
            &mut s,
            &request(
                "PUT",
                "/api/stulp/devices/d/group",
                r#"{"groupId":"absent"}"#
            ),
            &mut Env
        )
        .unwrap()
        .status,
        404
    );
    assert_eq!(
        json::text(s.document().record("devices", "d").unwrap(), "groupId"),
        "generated"
    );
    let expected = stulp_protocol::token::token("private-attach", "new-app").unwrap();
    let answer = web
        .handle(
            &mut s,
            &request("GET", "/api/stulp/attach-token/new-app", ""),
            &mut Env,
        )
        .unwrap();
    let token = json::parse(answer.body.bytes()).unwrap();
    assert_eq!(json::text(&token, "token"), expected);
    assert!(!json::boolean(&token, "known"));
}

#[test]
fn flow_catalog_uses_actual_listeners_device_filters_and_display_units() {
    let mut store=Store::open(r#"{"version":2,"system":{"units":{"temperature":"°F"}},"apps":[{"id":"a","enabled":true},{"id":"b","enabled":true}],"devices":[{"id":"lamp","appId":"a","driverId":"lamp","class":"light","capabilities":["onoff.left","measure_temperature","button"]},{"id":"other","appId":"b","driverId":"lamp","class":"light","capabilities":["onoff"]}]}"#.as_bytes(),Memory).unwrap();
    store.announce("a",json::parse(r#"{"id":"a","sdk":3,"version":"1","name":{"nl":"Lampen"},"drivers":[{"id":"lamp"}],"flow":{"actions":[{"id":"set","title":{"nl":"Instellen"},"args":[{"name":"device","type":"device","filter":"app_id=a&driver_id=lamp|socket&capabilities=onoff&future=ok"},{"name":"temperature","type":"number","units":"°C","min":0,"max":40,"step":0.5}]}],"triggers":[{"id":"press","title":"Pressed","args":[{"name":"device","type":"device","filter":{"app_id":["a"],"class":"light"}}]}]}}"#.as_bytes()).unwrap()).unwrap();
    store.set_app_status("a", "running").unwrap();
    let regs=json::parse(br#"{"a":{"flows":[{"id":"set","type":"action","runListener":true,"autocomplete":["name"]},{"id":"press","type":"device-trigger","runListener":true,"autocomplete":[]}]}}"#).unwrap();
    let before = store.manifest("a").unwrap().try_clone().unwrap();
    let cards = stulp_web::flow_cards(&store, &regs).unwrap();
    let set = json::array(&cards, "actions")
        .iter()
        .find(|v| json::text(v, "id") == "set")
        .unwrap();
    assert!(json::boolean(set, "available"));
    assert_eq!(json::array(set, "deviceIds").len(), 1);
    assert_eq!(json::array(set, "deviceIds")[0].as_str(), Some("lamp"));
    let arg = &json::array(set, "args")[1];
    assert_eq!(json::text(arg, "units"), "°F");
    assert_eq!(
        json::to_string(json::get(arg, "min").unwrap()).unwrap(),
        "32"
    );
    assert_eq!(
        json::to_string(json::get(arg, "max").unwrap()).unwrap(),
        "104"
    );
    let press = json::array(&cards, "triggers")
        .iter()
        .find(|v| json::text(v, "id") == "press")
        .unwrap();
    assert_eq!(json::text(press, "type"), "device-trigger");
    assert_eq!(json::array(press, "deviceIds").len(), 1);
    let held = json::array(&cards, "triggers")
        .iter()
        .find(|v| json::text(v, "id") == "capability.button.on_for")
        .unwrap();
    assert_eq!(json::text(held, "title"), "Knop bleef ingedrukt");
    assert!(
        json::array(&cards, "triggers")
            .iter()
            .all(|v| json::text(v, "id") != "capability.button.off_for")
    );
    let off = json::array(&cards, "actions")
        .iter()
        .find(|v| json::text(v, "id") == "capability.onoff.left.turn_off")
        .unwrap();
    assert_eq!(json::text(off, "title"), "Zet Aan/uit left uit");
    assert!(json::equal(&before, store.manifest("a").unwrap()));
    let unavailable = stulp_web::flow_cards(&store, &json::object()).unwrap();
    assert!(!json::boolean(
        json::array(&unavailable, "actions")
            .iter()
            .find(|v| json::text(v, "id") == "set")
            .unwrap(),
        "available"
    ));
}

#[test]
fn flow_threshold_roundtrip_and_scene_rename_preserve_canonical_values() {
    let mut s=Store::open(br#"{"version":2,"apps":[{"id":"a","enabled":true}],"devices":[{"id":"d","appId":"a","capabilities":["target_temperature"]}],"flows":[{"id":"f","name":"Wind","enabled":true,"nodes":[{"id":"t","step":{"appId":"stulp","cardType":"trigger","cardId":"capability.measure_wind_strength.rose_above","args":{"device":{"$device":"d"},"value":12}}}],"edges":[]}],"scenes":[{"id":"s","name":"Warm","kind":"switch","states":[{"deviceId":"d","capabilityId":"target_temperature","value":20}]}]}"#,Memory).unwrap();
    let mut system = json::object();
    json::set(
        &mut system,
        "units",
        json::parse(r#"{"temperature":"°F","wind":"Bft"}"#.as_bytes()).unwrap(),
    )
    .unwrap();
    s.system(system).unwrap();
    s.announce("a",json::parse(r#"{"id":"a","sdk":3,"version":"1","capabilities":{"measure_wind_strength":{"type":"number","units":"m/s"}}}"#.as_bytes()).unwrap()).unwrap();
    let web = Web::new("").unwrap();
    let response = web
        .handle(
            &mut s,
            &request("GET", "/api/manager/flow/flow/f", ""),
            &mut Env,
        )
        .unwrap();
    let mut shown = json::parse(response.body.bytes()).unwrap();
    let n = &json::array(&shown, "nodes")[0];
    let step = json::get(n, "step").unwrap();
    let args = json::get(step, "args").unwrap();
    assert_eq!(json::uint(args, "value"), 6);
    json::set(&mut shown, "name", json::string("Wind renamed").unwrap()).unwrap();
    let response = web
        .handle(
            &mut s,
            &request(
                "PUT",
                "/api/manager/flow/flow/f",
                &json::to_string(&shown).unwrap(),
            ),
            &mut Env,
        )
        .unwrap();
    assert_eq!(response.status, 200);
    let original = s.document().record("flows", "f").unwrap();
    let n = &json::array(original, "nodes")[0];
    let args = json::get(json::get(n, "step").unwrap(), "args").unwrap();
    assert_eq!(json::uint(args, "value"), 12);
    let response = web
        .handle(
            &mut s,
            &request("PUT", "/api/stulp/scenes/s", r#"{"name":"Still warm"}"#),
            &mut Env,
        )
        .unwrap();
    assert_eq!(response.status, 200);
    assert_eq!(
        json::uint(
            &json::array(s.document().record("scenes", "s").unwrap(), "states")[0],
            "value"
        ),
        20
    );
    let shown = json::parse(response.body.bytes()).unwrap();
    assert_eq!(json::uint(&json::array(&shown, "states")[0], "value"), 68);
}

#[test]
fn capability_defaults_match_go_and_custom_values_keep_their_types() {
    let reference = json::parse(include_bytes!("../data/capabilities.json")).unwrap();
    for (id, expected) in reference.as_object().unwrap().iter() {
        let mut actual = json::object();
        stulp_core::capability::defaults(&mut actual, id).unwrap();
        assert!(
            json::equal(&actual, expected),
            "{id}: {actual:?} != {expected:?}"
        );
    }
    let mut store = Store::open(br#"{"version":2,"apps":[{"id":"a","enabled":true}],"devices":[{"id":"d","appId":"a","driverId":"lamp","capabilities":["custom_boolean","custom_number","meter_power","air_quality_state"]}]}"#, Memory).unwrap();
    store
        .observe(
            "a",
            "d",
            json::parse(br#"{"custom_boolean":true,"custom_number":12}"#).unwrap(),
            true,
            "",
        )
        .unwrap();
    let object = stulp_web::device_object(&store, "d").unwrap();
    let caps = json::get(&object, "capabilitiesObj").unwrap();
    assert_eq!(
        json::text(json::get(caps, "custom_boolean").unwrap(), "type"),
        "boolean"
    );
    assert_eq!(
        json::text(json::get(caps, "custom_number").unwrap(), "type"),
        "number"
    );
}
#[test]
fn nested_plugin_measures_use_display_units_without_touching_plain_numbers() {
    let mut store = store();
    store
        .system(json::parse(r#"{"units":{"temperature":"°F"}}"#.as_bytes()).unwrap())
        .unwrap();
    let input = json::parse(r#"{"nested":[{"$measure":21.5,"units":{"en":"°C"}},78,{"value":21.5,"units":"°C"},{"$measure":78,"units":"%"},{"$measure":"invalid","units":"°C"}]}"#.as_bytes()).unwrap();
    let output = stulp_web::show_measures(&store, &input).unwrap();
    let values = json::array(&output, "nested");
    assert_eq!(json::text(&values[0], "text"), "70.7 °F");
    assert!(json::equal(
        json::get(&values[0], "measured").unwrap(),
        &json::parse(b"21.5").unwrap()
    ));
    assert_eq!(json::text(&values[0], "canonical"), "°C");
    assert_eq!(json::text(&values[3], "text"), "78%");
    for index in [1, 2, 4] {
        assert!(json::equal(
            &values[index],
            &json::array(&input, "nested")[index]
        ));
    }
}
