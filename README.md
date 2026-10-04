# Stulp

*Your apps. Your devices. Your home.*

A small controller for the home, written in Rust. Install an app, pair a device,
control it, configure it, and connect it to other devices in a Flow. That loop
is the product; everything here exists to make it calmer or more reliable.

Stulp keeps no database engine. All platform state lives in one portable JSON
document, because the data is small, the questions are simple, and there is
deliberately no history to query. A sensor reporting a temperature costs no disk
at all.

## Apps are plugins

An app is an ordinary program: one binary, started by Stulp, holding one end of
a private socket over which the two speak a small length-prefixed protocol.

```rust
fn main() -> std::process::ExitCode {
    stulp_plugin_host::run(stulp_lamp::Lamp::default)
}
```

One process per app, so a plugin that hangs or crashes takes nothing else down
and there is no way for one app to reach another. Reads come from a snapshot
Stulp pushes at the handshake and keeps current, so asking a device its name is
a lookup and not a round trip. On HopOS every app is its own slot and attaches
over the private slot network instead; nothing past the handshake differs.

The SDK (`crates/sdk`) and every included app are `no_std` with fallible
allocation and no unsafe code. There is no Tokio, no SQLite, no `Arc` and no
application mutex: one owner task holds the document and the connections, and
fixed pools of workers exchange messages with it. `crates/platform` is the only
place that touches the host's libc.

## Matter, without a radio

The experimental native companion under `ios/` lets an iPhone scan a
factory-new Matter accessory and provision its Wi-Fi or Thread network
directly. iOS supplies the temporary BLE path; Stulp then commissions its own
fabric over IP, so there is no Homey or Apple Home pairing code to copy. Its
Swift and HTTP path are tested; factory-new hardware validation is still the
release gate. See [`ios/README.md`](ios/README.md) for signing and device setup.

Matter is multi-admin too, so an accessory already commissioned in Apple Home
can still be shared with Stulp over the network. For Thread, an Apple TV,
HomePod or another border router keeps carrying the same network iOS selected
after the iPhone's one-time setup. Stulp itself needs no Matter or Thread radio.

That covers TLV, onboarding codes, DNS-SD discovery, SPAKE2+ and CASE, real
attestation checks, subscriptions, and a mesh view built from what the nodes
themselves report. Native BLE, Zigbee, Z-Wave and RF stacks are out of scope on
purpose: Stulp is an on-network controller and relies on the ecosystem that is
already in the house.

## Layout

| | |
|---|---|
| `crates/core` | the one JSON document, its store and the bounded parser |
| `crates/protocol` | the wire protocol between controller and app, with frame caps |
| `crates/runtime` | the app-side runtime: snapshot, devices, callbacks |
| `crates/controller` | the owner of document, apps, pairing, Flows, scenes and media |
| `crates/web` | the keyed Manage interface, its private browser API, SSE and MCP; `ui/` holds the assets |
| `crates/host` | `stulp-host`: the native controller binary, CLI, supervision, backup |
| `crates/sdk` | what a plugin is written against |
| `crates/plugin-host` | the native plugin process: HTTP, TLS, DNS, streams, media |
| `crates/transport` | framed attach connections and the server key for Lean's TLS 1.3 (`leantls`) |
| `crates/platform` | the checked libc calls, isolated and Miri-tested |
| `crates/webpush` | VAPID and encrypted Web Push, with vendored crypto |
| `crates/hopos`, `hopos-app`, `hopos-plugins` | the HopOS slot controller, its image and the per-plugin and all-plugins images |
| `plugins/<name>` | one app each: Rust source next to its `app.json`, settings pages and pair views |
| `vendor/types` | Hop's `types` with in-place JSON edits (see `vendor/PROVENANCE.md`) |
| `tests/` | process, media and QEMU tests against the real binaries |

## Building and running

Rust 1.93.0 is pinned in `rust-toolchain.toml`. HopOS, Lean and Hop come from
their tagged releases; no sibling checkout is needed.

```sh
./build.sh
STULP_TOKEN=EEN-LANGE-WILLEKEURIGE-SLEUTEL target/release/stulp-host \
  --document stulp.json serve --listen 127.0.0.1:8080
```

Open Manage at `http://127.0.0.1:8080/EEN-LANGE-WILLEKEURIGE-SLEUTEL`. The same
key exposes the stateless MCP server at `/mcp/EEN-LANGE-WILLEKEURIGE-SLEUTEL`.
Visiting the Manage URL establishes an HttpOnly browser session; there is no
API-key field in the interface and the private browser API does not accept
Bearer tokens.

`build.sh` builds the controller and every app in `plugins/*` into
`target/release/`: `stulp-host` and `stulp-<name>` for each plugin. The other
targets:

| | |
|---|---|
| `./build.sh linux` | static `-linux-arm64` and `-linux-riscv64` binaries in `out/`, linked with the toolchain's own lld |
| `./build.sh hopos` | stripped HopOS slot images in `out/`: `stulp-<arch>-tamago.elf`, `<app>-<arch>-tamago.elf`, `all-plugins-<arch>-tamago.elf` |
| `./build.sh check` | formatting, strict clippy, workspace tests, real controller/plugin process tests, `no_std` checks for ARM64 and RISC-V |
| `./build.sh qemu-controller` | boots controller and a plugin in separate HopOS slots, including a cold restart |
| `./build.sh qemu-persist`, `qemu-ipv6` | the storage probe and the IPv6/NDP/multicast test between slots |

Stulp starts every app itself. An app that cannot be started that way can start
first and announce itself instead:

```sh
target/release/stulp-host --document stulp.json serve --attach /tmp/stulp-attach.sock
STULP_ATTACH=/tmp/stulp-attach.sock target/release/stulp-nibe
```

For an app in its own pod or slot there is `--attach-port`, which needs a
per-app token (`stulp-host attach-token APP_ID`) because a port has no uid to
ask about. The token is never sent: Stulp opens with a nonce and the app
answers with an HMAC of it, both ways. TLS on top is what makes the traffic
private, and `--attach-port` insists on it unless you say `--attach-plaintext`.

## Releases

`tools/release.sh` builds the four flavours of every binary and publishes them
as assets on the rolling GitHub tag `apps`, which is the update channel the
nodes' startup files point at. With a version argument it also creates the
immutable release of that version; the version must match `Cargo.toml`.

[ARCHITECTURE.md](ARCHITECTURE.md) describes the controller, the plugins, the
HopOS runtime, limits and what the test suites do and do not prove.

## License

Stulp is available under the [MIT License](LICENSE).
