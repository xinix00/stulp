//! All response-table checks run on one thread, matching the executor invariant.
#![allow(clippy::unwrap_used, clippy::panic)]
use stulp_controller::{Inbox, Reply};
use stulp_hopos::replies::Sender;
use stulp_web::Response;
#[test]
fn bounded_leases_cancel_and_do_not_deliver_late_results_to_reused_slots() {
    let (stale, first) = Sender::channel().unwrap();
    stale.send(Response::error(201, "first").unwrap()).unwrap();
    assert!(
        stale
            .send(Response::error(202, "overflow").unwrap())
            .is_err()
    );
    assert_eq!(first.receive().unwrap().unwrap().status, 201);
    drop(first);
    let (current, second) = Sender::channel().unwrap();
    assert!(stale.send(Response::error(500, "late").unwrap()).is_err());
    assert!(second.receive().unwrap().is_none());
    current
        .send(Response::error(204, "current").unwrap())
        .unwrap();
    assert_eq!(hostnet::block_on(second.wait()).unwrap().status, 204);
    let mut routes = Vec::new();
    while let Ok(route) = Sender::channel() {
        routes.push(route);
    }
    assert_eq!(routes.len(), 127);
    drop(routes);
    drop(second);
    let (_sender, receiver) = Sender::channel().unwrap();
    assert!(receiver.receive().unwrap().is_none());
}
