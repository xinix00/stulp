//! QEMU-bewijs: NDP, unicast en ff02-multicast door twee echte HopOS-slots.
#![cfg_attr(target_os = "none", no_std, no_main)]
use applib::{
    App, EXEC,
    appnet::{self, Endpoint6, Udp6Socket},
};
use core::time::Duration;
applib::main!(probe);
#[cfg(not(target_os = "none"))]
fn main() {}
const GROUP: [u8; 16] = [255, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 251];
async fn run(app: &'static App) -> Result<(), &'static str> {
    let net = appnet::up(app).map_err(|_| "network")?;
    net.join_group6(GROUP).map_err(|_| "join")?;
    let mut socket = Udp6Socket::bind(5540).map_err(|_| "bind")?;
    socket.set_timeout(Some(Duration::from_secs(30)));
    let local = Endpoint6 {
        ip: net.ipv6_addresses().map_err(|_| "addresses")?.0,
        port: socket.local().map_err(|_| "local")?.port,
    };
    app.log(format_args!(
        "STULP_IPV6_READY {}",
        core::net::Ipv6Addr::from(local.ip)
    ));
    let mut buf = [0; 64];
    if app.env("MODE") == Some("server") {
        for expected in [b"unicast".as_slice(), b"multicast".as_slice()] {
            let (n, from) = socket.recv_from(&mut buf).await.map_err(|_| "receive")?;
            if &buf[..n] != expected {
                return Err("payload");
            }
            socket.send_to(from, expected).await.map_err(|_| "reply")?;
        }
        app.log(format_args!("STULP_IPV6_SERVER_PASS"));
    } else {
        let ip = app
            .env("PEER")
            .ok_or("peer")?
            .parse::<core::net::Ipv6Addr>()
            .map_err(|_| "peer parse")?
            .octets();
        for (ip, message) in [
            (ip, b"unicast".as_slice()),
            (GROUP, b"multicast".as_slice()),
        ] {
            socket
                .send_to(Endpoint6 { ip, port: 5540 }, message)
                .await
                .map_err(|_| "send")?;
            loop {
                let (n, from) = socket
                    .recv_from(&mut buf)
                    .await
                    .map_err(|_| "reply receive")?;
                if from.ip == local.ip {
                    continue;
                } // De eigen multicastkopie is geldig.
                if &buf[..n] != message {
                    return Err("reply payload");
                }
                break;
            }
        }
        app.log(format_args!("STULP_IPV6_CLIENT_PASS"));
    }
    Ok(())
}
async fn probe(app: &'static App) {
    if let Err(e) = run(app).await {
        app.log(format_args!("STULP_IPV6_FAIL {e}"));
    }
    loop {
        EXEC.get().after(Duration::from_secs(60)).await;
    }
}
