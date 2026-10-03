# Stulp architecture

Stulp is a native and HopOS home controller with ten plugins, written in Rust.
The shared controller, external-slot runtime and per-node Matter command
scheduling are implemented. Platform limits and physical-device validation are
listed below; build success does not establish compatibility with every device
firmware. Tests use disposable state, never a live `stulp.json`.

TaHoma's live `401 AUTHENTICATION_ERROR` can mean "Too many requests", rather
than rejected credentials. The Somfy plugin distinguishes that response,
waits at least 15 minutes (or a longer numeric `Retry-After`) before retrying,
and backs off other failed logins from one minute to 15 minutes. Polls, commands
and the settings connection test respect the cooldown; device status and logs
show a sanitized reason without the account echoed by TaHoma. The settings API
exposes `retryAfter` in seconds. Contract tests verify quiet polling and recovery;
the provider's live lockout must expire before connectivity can be confirmed.

## Build and run

Rust 1.93.0 / edition 2024 is pinned. Hop, Lean and HopOS sync dependencies use
tagged git revisions in `Cargo.lock`; the IPv6 SDK overlay is pinned by local
source hashes in `vendor/PROVENANCE.md`. The SDK uses `../lean/leannet`
from the sibling Lean checkout directly, without a netstack copy in Stulp. No Tokio,
SQLite, Arc or application mutex. Logic crates use `no_std`, fallible allocation
and forbid unsafe code. `stulp-platform` isolates the host's checked libc ABI
calls. Pinned, vendored crypto provenance is in `crates/webpush/PROVENANCE.md`.

```sh
./build.sh
./build.sh check
```

Release output: `target/release/stulp-host` and `stulp-{virtualdevices,weather,
somfy,nibe,spotify,notify,wiim,sigenergy,unifi,matter}` in the same directory.
Use a separate document for development:

```sh
STULP_TOKEN=local-development-key target/release/stulp-host \
  --document /tmp/stulp-rust-development.json serve --listen 127.0.0.1:8080
```

Open `http://127.0.0.1:8080/local-development-key`. Original HTML, JavaScript, CSS,
PWA and plugin UI assets are embedded. The existing HttpOnly browser session and
same-origin write checks protect the API. `check`, `apps` and `devices` CLI
commands also accept `--document`; without it the default is `stulp.json`.

The host supervises installed local bundles and connects them through a private
0700 directory and 0600 Unix socket. `serve --attach SOCKET` also accepts externally
started apps. Unix peers must have the same kernel-verified effective UID; plugins
use `STULP_ATTACH=/absolute/socket` without a token, matching Go. Explicit TCP attach
uses `--attach-port ADDRESS --attach-plaintext` and `STULP_ATTACH_TOKEN`; both peers
prove the token with fresh nonces. `--tls-cert FILE --tls-key FILE` enables HTTPS
and encrypts the TCP attach listener unless `--attach-plaintext` is explicit.
Remote plugins default to TLS, with `STULP_ATTACH_CA` for a custom trust root;
`STULP_ATTACH_INSECURE=1` skips only chain/name/date verification, matching the Go
configuration, while still checking TLS signatures and the separate HMAC proof.
Certificate/key mismatches fail before listening. The fixed owner/pools enforce
a ten-second handshake deadline; an idle peer does not block the app controller.

The Stulp-local TLS extension supports TLS 1.3/AES-128-GCM, X25519/P-256 ECDH,
P-256/P-384/Ed25519/RSA signing, PKCS#8/SEC1/PKCS#1 keys and verified DNS/IP SANs.
See `crates/tls/PROVENANCE.md` for dependencies and explicit TLS-stack limits;
TLS 1.2, P-521 and HelloRetryRequest are not implemented. HTTPS uses the same
large-upload, backup, SSE and media paths as HTTP.

CLI commands include `check`, `apps`, `devices`, `install`, `uninstall`,
`attach-token` (including rotation), `add-device`, `run --once`, `inspect`,
`pair-list`, `invoke`, `flow`, `backup` and `restore`. Run `--help` for their
arguments. Explicit CLI execution may run a disabled app once without changing its
saved enabled state. The document lock excludes a second writer while a command is active.

## Controller and browser integration

One controller task owns the document, live observations, app connections,
pair sessions, Flow runs and media tickets. Eight fixed HTTP workers exchange
messages with that owner. Each connection handles one HTTP request (with streaming
SSE/media retained); large restore uploads have a separate bounded framing parser. App/plugin state is never shared behind a lock.

- Versioned JSON, durable atomic publication, revisions, groups and ordering,
  uninstall cleanup, transient device values/store keys, unit conversion,
  scene baseline bookkeeping and CRC/generation A/B storage are implemented.
- Authenticated app snapshots, initialization, owned mutations, heartbeat,
  bounded callbacks, disconnect cancellation and state pushes work over real
  loopback connections. A lagging app reconnects for an authoritative snapshot.
- Browser pairing starts/emits/closes sessions and adopts candidates with driver
  defaults. The device snapshot precedes initialization. An init failure rolls
  back the stored device; the plugin connection remains usable. Duplicate
  candidate retries reuse an existing device, while an adoption still in progress
  returns conflict. Removing a device first invokes the plugin for external
  cleanup; an offline plugin cannot prevent local deletion.
- Browser device settings invoke a bounded plugin callback before the controller
  merges the patch into the latest device record. Unchanged patches cause no
  device command. Concurrent settings/unpair operations are rejected; unrelated
  renames survive. WiiM addresses and Sigenergy unit IDs are validated; UniFi
  camera microphone writes return failures to the browser without blind retries;
  Matter runs its validated attribute writes. Physical writes cannot be rolled
  back atomically with document storage; Matter retains individually confirmed
  setting writes if a later network operation fails.
- Settings/pairing assets, bridge injection, custom pair views, manifest API
  proxies and registration/discovery queries invoke the actual plugin. Local bundle
  pages and locales also work while the plugin is stopped, with path/symlink bounds.
- Manual Flow tests and automatic device/plugin events execute the same graph.
  Automatic runs honor enabled state, trigger/device filters, threshold crossings
  and plugin trigger listeners. Token substitution retains typed values, including
  exact 64-bit integers. Initial device observations establish a baseline rather
  than triggering automations; unchanged observations do not duplicate events.
- Flow conditions, capability commands, notifications, bounded delays, callback
  errors and history persistence are wired. Runs retain an immutable definition,
  execute a shared successor once and have a 45-second deadline. Newer starts
  win when recording history. Four runs execute concurrently; 256 automatic
  trigger messages can wait. A full trigger queue returns an error rather than
  silently dropping an accepted event.
- Registered media slots resolve through the owning plugin. The controller
  streams HTTP(S) bytes without codecs or exposing private source URLs. Media
  and SSE admission retain two HTTP workers for ordinary requests.
- Notify image choices use the live image registry. Shared image URLs retain only
  a resolver, expire after 15 minutes and are never persisted. Eight tickets may
  wait; oldest tickets are evicted. A valid ticket can be fetched without a
  browser cookie, and only that fetch requests a fresh snapshot from the plugin.

- Local child supervision covers startup, observed exit, bounded restart delay,
  enable/disable, offers, install/restart/uninstall and confirmed termination
  before deleting owned bundles. SIGINT/SIGTERM reap owned children.
- The Flow catalog includes original bilingual metadata, plugin cards, derived
  capability cards, device filtering and autocomplete. Scheduled triggers use IANA
  timezones, DST and sunrise/sunset calculations checked against Go. Stability
  watches cancel when values change. Text tokens use display units; typed numeric
  arguments retain canonical values. API `$measure` objects get display metadata.
- Older plugins fall back from grouped commands to ordered individual commands
  only after an explicit unknown-method response, within the original deadline.
  Timeouts and transport failures never cause uncertain commands to be repeated.
- Scenes save restore baselines before commands, send groups per device, retain
  failed restoration values and detect manual overrides. Device-reference rewrites
  include active scene restore values and Flow hidden state in one durable update.
- Optional statistics use three fixed rings per capability. MCP exposes the
  original 17 tools with actual device/Flow/scene callbacks, authentication,
  Origin checks, input/output limits, admission limits and timeouts.
- Manage bootstrap and overview/SSE use compact tile metadata. Automation views
  carry all capabilities without private settings/data. SSE supports manager
  filters while retaining reload markers. Plugin HTML gets its locale with English
  fallback, escaped before script injection.
- CLI and browser ZIP backup/restore include local app bundles and private state.
  Go-produced deflated archives and Rust-produced stored archives interoperate.
  Restore validates paths, file types, ownership, manifests and CRCs in private
  staging before stopping children, publishing state and restarting installed apps.
  A failed publication restores prior bundles; the previous document is retained.
  Configuration-only app entries reuse matching installed local bundles, keeping
  the imported settings and enabled state. Bundles supplied by the archive retain
  precedence; paths inside a replaced bundle tree are never reused.
  Browser uploads stream up to 512 MiB, including chunked bodies, without loading
  the entire archive into RAM. ZIP expansion is limited to 2 GiB / 100,000 entries.

Only configuration, explicit persistent plugin state and Flow history cause disk
writes. Device values and availability remain transient. Matter writes report
markers/settings only when they actually change.

## Plugin implementations

| Plugin | Rust implementation and validation |
| --- | --- |
| Virtual devices | Persistent switches, isolated pairing sessions; real browser-API create/control/dedup/close and restart tests. |
| Weather | Open-Meteo search/forecasts, canonical units, conditions and threshold events; midnight, WMO and threshold tests. |
| Somfy | Private account/cookie sessions, seven covering drivers, execution cancellation and scenarios; auth refresh, heartbeat, command/storage failure and actual UI/API proxy tests. |
| Nibe | PKCE/machine login, token rotation, S/F parameter maps, capabilities, polling and energy splitting; OAuth, state and persistence tests. |
| Spotify | PKCE, Connect discovery/control, changed IDs, polling, search and playback; identity, search, payload and redirect tests. |
| Notify | Durable VAPID, encrypted Web Push, muting, expired subscriptions and controller image integration; validated during the port by independent Go decryption and signature verification. |
| WiiM | UPnP/DIDL, commands, SSDP/manual pairing and recovery/diagnostics; original XML fixtures, heartbeat/pairing tests and independent local TLS peer. |
| Sigenergy | Five Modbus drivers, grouped reads/fallback, discovery, mySigen tokens and Gateway writes; register scales, polarity, token persistence and no-write-retry tests. |
| UniFi | Five drivers, REST, two reconnecting WebSockets, events, camera snapshots and RTSP/RTSPS-to-fMP4 video; real controller/plugin test with synthetic TLS Protect and RTSP camera, plus ffmpeg decoding; the mux was compared against the original Go implementation during the port. |
| Matter | Durable fabric, commissioning, operational CASE sessions, subscriptions, grouped commands, settings writes, reports/events, UI, scan, diagnostics and Thread mesh; actual plugin callbacks; validated during the port against independent Go cryptographic and device peers. |

Matter restoration also covers homes with many offline nodes: all startup
callbacks preempt background maintenance, and reconnect work chooses the oldest
due deadline so early offline nodes cannot starve later devices. Initial reconnect
failures are logged. The IM decoder accepts peer revision metadata without requiring
it to equal the sender's revision 1, matching the original Go decoder and the
[CHIP message parser](https://github.com/project-chip/connectedhomeip/blob/master/src/app/MessageDef/MessageParser.cpp).
Revision field types and the actual message structures remain validated; regression
fixtures cover reports and command responses with multiple peer revisions.

### Matter

The runnable owner connects onboarding, co-operative SPAKE2+/PASE, CASE,
attestation/CSR checks, root/NOC issuance, commissioning completion, DNS-SD,
endpoint inventory/model and capability mapping. Failed operational discovery
retries CASE; it does not blindly repeat AddNOC. Persistent credentials are
confirmed by the controller before use. Existing Go PKCS#8/X.509 fabric state
imports without replacing identity; corrupt state fails initialization.

MRP, persistent IPv4/scoped-IPv6 UDP, chunked IM reads/writes/timed invokes,
subscriptions and report watchdog/backoff run under the main protocol owner. Event
markers are persisted before triggering Flows. Grouped color/level/power plans
and full-patch sensitivity validation match the Go logic. DAC/PAI and operational
certificates are checked; compact operational certificates support Stulp's
root/direct-NOC profile and reject unrepresentable signed TBS data.

A bounded cooperative SDK job pump keeps heartbeats, local snapshots, progress,
UI assets and cancellation responsive during protocol work. Two additional scoped
command owners keep independent CASE sessions: commands to one physical node are
serialized, while commands to another can proceed. Lifecycle/settings barriers wait
for active commands and prevent later commands from overtaking them. The main owner
retains commissioning, fabric counters, subscriptions and background maintenance.
Snapshots follow authoritative state revisions; no application state is shared behind
a lock. Each scoped job uses a separate platform-seeded ChaCha20 entropy stream,
rekeyed with fresh parent entropy, so nested jobs cannot exhaust a seed queue.
A transport interruption ends all owners and forces reconnect. Cancelled discovery
and hostname results are consumed without contaminating the next scan.

A scheduler test proves same-node ordering and cross-node progress. An independent
Go Matter device proves that an explicit command to a healthy node completes while
another explicit command is still awaiting a silent node, including after orphan
fabric cleanup. This is bounded concurrency (two active command nodes), not an
unbounded worker per device.

Diagnostics read Basic Information, General, Thread and Wi-Fi diagnostics with
strict typed bounds. Mesh scans grow through per-node work and combine neighbor
and border-router links. Network discovery visits active LAN interfaces, excludes
loopback/point-to-point VPNs and preserves IPv6 interface scopes and binary TXT.

Existing model versions refresh over the existing CASE session before subscription,
retaining names, groups and credentials. The independent Go device test exercises
that migration. Previously advertised luminance capabilities upgrade at startup.
No-route errors use one shared retry window with jitter and two minutes of grace;
normal device timeouts retain their individual retry state. Repeated model-refresh
warnings are counted per node and logged once. Discovery supports Go's 30-second
maximum and WiiM SSDP also visits each active LAN interface.

Hostname resolution runs away from the controller pump; a ten-second SDK deadline
keeps status/heartbeat callbacks responsive. macOS uses the original dns-sd fallback
when application multicast cannot start/send. Binary TXT values pass through the
same DNS collector; only matching operational nodes resolve missing addresses.
The host system resolver itself can outlive its SDK deadline; its late response is
discarded before the next query.

The controller consolidates legacy split native endpoints at authenticated Matter
attach, before the welcome snapshot. One durable transaction per physical node
preserves the main identity/name/group, pairing credentials, endpoint routes,
Flow references and active scene restore values. It also remaps queued events
and live capability revisions. Newly supported luminance capabilities are mapped
on their original endpoint before combining cluster metadata, in the same commit.
Bridge children and other apps remain separate.
Failure injection verifies rollback; a real controller test verifies startup order
and rejects migration for disabled/unauthenticated attaches. This uses a portable
controller-owned migration, without adding a plugin create/delete RPC.

Physical-device validation remains pending; the independent peers establish protocol
interoperability, not compatibility with all firmware or LAN topologies.

### UniFi media

The plugin selects the video SDP track, negotiates DESCRIBE/SETUP/PLAY, frames
interleaved RTP/RTCP, sends OPTIONS keepalives and bounds setup/read timeouts.
H.264 single NAL/STAP-A/FU-A and AV1 aggregation/fragmentation assemble complete
frames. Packet types, lengths, sequence gaps, padding and extensions are checked.
MP4 timestamps begin at zero and survive 32-bit RTP wrap. No transcoding is done.
As in Go, this path targets camera streams without B-frame reordering and the
existing 8-bit AV1 configuration; audio and recording playback are outside it.

One lazy host media worker owns four live sources, up to four pending snapshots
and eight viewers. Frame buffers have local IDs and owner-managed references;
there is no Arc or thread per viewer. The GOP cache is at most 8 MiB per camera;
slow viewers wait for a new keyframe after missing a frame. Thirty seconds without
viewers closes the camera. The shared frame depot caps at 32 MiB and the eight-entry
command queue accepts frames of at most 8 MiB. Resource exhaustion fails the source
explicitly. Snapshots are at most 4 MiB, are fetched into a bounded plugin buffer,
and move into the one-shot media response. This differs from Go's fully streaming
snapshot fetch and remains a memory optimization opportunity.

Local device TLS deliberately accepts the device's self-issued certificate while
verifying handshake signatures. Public cloud APIs keep normal chain/name checks.
Lean supports TLS 1.3; actual WiiM/UniFi firmware compatibility needs device tests.
The native media listener binds loopback, so native plugins and controller share a
host/network namespace. The HopOS adapter publishes on the private slot address,
allowing the controller to proxy a plugin running in a separate slot.

## Remaining application parity

- The application now serves HTTPS and TLS attach. The bounded TLS extension
  does not implement every algorithm/version of Go's general TLS stack (see above).
- Matter uses two concurrent command owners as described above; real accessories
  and the iOS provisioning path still require hardware validation.
- IPv6 UDP, NDP, SLAAC and PIO/RIO routing are now ported into Lean and the
  HopOS SDK. Stulp uses IPv4/IPv6 mDNS, numeric IPv6 scopes and A/AAAA resolution.
  Real Thread-border-router and NIC validation remain a hardware gate; emulator
  success does not prove that a physical NIC passes the required multicast and
  synthetic slot MACs. The profile deliberately excludes TCPv6, fragmentation,
  DHCPv6 and MLD, matching Go's existing Lean contract.

## HopOS runtime

`./build.sh hopos` builds `stulp-hopos-app` and ten `stulp-<name>-hopos`
executables under `target/<target>/release` for ARM64 and RISC-V, and packages
them into `out/` under the names the nodes' startup files use. Hop starts each
plugin as an external slot; the controller does not fork native bundles. Both
platforms use the same Flow, scene, pairing, catalog, MCP and browser UI services.

`stulp-all-plugins-hopos` combines all ten plugins into one image, as the old
`all-plugins-*-tamago.elf` did. It shares one network and executor, with separate
request/stream queues and reconnect generations per plugin. Four I/O workers per
plugin start with the bundle; the eight streaming workers start only when used
(UniFi), keeping the bundle within the SDK's 64-task budget. The existing
`STULP_ATTACH_SECRET` derives a separate token per app; `STULP_TOKENS` overrides
individual app tokens. Controller, bundle and tunnel jobs use
`"cores":1,"tags":{"sharegroup":"huis"}` to share the one app core.

`./build.sh hopos` strips each image with the toolchain's `rust-objcopy`
(`out/stulp-<arch>-tamago.elf`, `out/<name>-<arch>-tamago.elf` and
`out/all-plugins-<arch>-tamago.elf`). The separately built Cloudflare Lean ELF
needs the same treatment. This removes Rust debug and symbol tables while
retaining the four HopOS ABI patch symbols. Merely stripping debug information leaves large symbol tables
that exhausted the LicheeRV loader's memory. Loadable code and data are unchanged.
The compact controller, ten-plugin pack and tunnel were verified running together
on the physical LicheeRV with HopOS 3.0.3 on 2 October 2026.

For an explicit disposable test on a board reporting `HOPOS_DISK_NONE`, set
`STULP_STORAGE=memory`. It logs `STULP_STORAGE_VOLATILE` and loses configuration
on restart; export a backup before stopping. The default remains durable disk
storage, and a failed disk does not silently fall back to memory. HopOS uses the
bundled timezone data when no mounted TZif is available.

Mount a persistent volume at `/data`. The controller defaults to
`/data/stulp.json` (`STULP_DOCUMENT` overrides it), requires `STULP_TOKEN`, and uses
`ER_PORT_HTTP=8080` and `ER_PORT_ATTACH=7000` by default. An initial
`STULP_ATTACH_SECRET` can seed an empty store; existing persisted credentials win.
Plugins require `STULP_ATTACH=<controller-private-slot-ip>:7000` and the corresponding
`STULP_ATTACH_TOKEN`. This mutual-HMAC connection is intended for the private slot
network and is plaintext, matching the original HopOS arrangement. On kernels
without platform entropy, startup requires an independently generated, secret,
fresh 32-byte base64 `STULP_ENTROPY_SEED`; the QEMU harness supplies one per job.
Do not copy the test credentials or entropy into a deployment.

Eight fixed HTTP workers, one controller owner and bounded reply leases serve
ordinary API calls, SSE, media and restore uploads. Fixed plugin workers handle
HTTP/TLS, framed TCP, A/AAAA DNS, IPv4/IPv6 discovery, eight duplex streams and bounded media.
Attach generations cancel abandoned work and prevent replies crossing reconnects.
Public HTTPS checks the certificate chain and name; explicit device-certificate
mode retains signature/record checks. Device TLS compatibility follows the TLS
limits above.

External-slot backups keep the Go ZIP metadata/document contract, accept stored
and deflated entries, validate paths and CRCs, and publish restores atomically.
Uploads accept fixed or chunked bodies up to 80 MiB after authentication; the JSON
document remains capped at 64 MiB. Archives containing native executable bundles
are rejected explicitly. IANA timezone data is bundled with its source/license;
`/data/zoneinfo/<name>` can supply an override.


## Limits and storage

Documents accept **64 MiB**, matching Go backup/restore; app frames accept **8 MiB**
and ordinary API JSON remains bounded to **1 MiB**. A Stulp-local adaptation of the
pinned Hop parser preserves its value/error contracts while making the input budget
explicit. Its original MIT + Commons Clause license is retained in
`crates/core/HOP-LICENSE`. The IPv6 adapter also exists in the shared SDK source. Binary media
uses a separate path.
Other limits: 4,096 records/collection, 128 Flow nodes, 256 edges, 64 invalidation
records, 32 attached apps, 64 callbacks/app and 64 packets/4 MiB queued per app.
Pairing has 32 sessions and 64 pending lifecycle operations. Overflow is explicit.

File persistence syncs a private temporary file, renames within the same directory
and syncs the directory. A directory-sync failure after rename reports degraded
durability while retaining the published state. An exclusive sidecar lock excludes a second writer across document renames.

## Reproducible validation

`./build.sh check` runs formatting, strict clippy, workspace tests, real
controller/plugin process tests and `no_std` library checks for ARM64 and RISC-V.
Process tests include ZIP round trips (stored and deflated entries), CLI
management and real Unix/TCP plugin connections. Archive interoperability
includes a document larger than 2 MiB. The independent Go protocol and crypto
peers that validated the port (TLS, Web Push, Matter PASE/CASE/certificates,
RTSP mux) are no longer part of this tree; the TLS crate still skips its
optional Go peer tests when `go` is absent. Sigenergy scans test all 255 units over more than ten minutes of
simulated time while retaining heartbeats. Media player checks require ffmpeg,
ffprobe and openssl; their tests explicitly skip when those tools are absent.

Tests cover failed-save rollback, restart/secrets separation, exact numeric
identity, revisions, graph ordering, scene baselines, torn A/B writes, fragmented
frames, attach ownership, timeouts, UI auth, SSE, pairing rollback/dedup/unpair,
settings validation/concurrency/failure persistence, automatic Flows, media tickets
and private proxying. TLS tests use OpenSSL peers (and the optional Go peer when
`go` is installed), both attach directions, IP SANs, cold restart, stalled
handshakes, SSE and large Content-Length/chunked restores. Native plugin logs filter by --log-level and
retain bounded partial lines and UTF-8 across buffer boundaries, including a child's
final unterminated error. Optional locale read errors fall back to English. MCP
execution projections use Go's separate 512-value/8-KiB result budget. The real UniFi process test
fetches a synthetic JPEG and decodes 25 proxied H.264 frames. During the port,
independent Go peers validated the RTSP mux for both codecs across RTP wrap,
100,000-iteration PASE, CASE, certificates, orphan-fabric cleanup,
commissioning/reconnect/control/subscription/removal, foreground control during
background recovery, 32 endpoint combinations and 15 command plans. Those peers
are not part of this tree; the Rust fixtures they produced remain under
`plugins/matter/tests/fixtures`.

`./build.sh qemu-persist` builds the isolated storage probe and uses prebuilt
HopOS/Hop artifacts read-only (`STULP_QEMU_KERNEL` / `STULP_QEMU_HOP` override them).
It creates a disposable volume, writes a 192 KiB Stulp document through the real
A/B backend, SIGKILLs QEMU and verifies the same document after cold boot. The run
passed on this checkout; logs and artifact hashes are in `target/qemu-stulp-persist`.
The probe binary also builds for RISC-V; that target has not been booted here.

Storage uses HopOS SDK alpha.18, which already provides the parked stack and sync
RPC. Cancellation and uncertain sync failures poison that I/O owner; readback of
cache bytes cannot masquerade as a successful durability barrier. A native test
uses the actual SDK stack with asynchronous injected failures and verifies that
neither the document nor its event cursor changes after the failed commit.

`./build.sh miri` checks the isolated platform wrapper under macOS/Linux Miri
layouts using nightly, with the sysroot built outside the application vendor tree.
OS ABI calls have native tests; no live LAN discovery, real devices/accounts,
deployment or performance claim is implied by these tests.

`./build.sh qemu-controller` boots the actual controller and Rust virtual-device
plugin in separate slots. It checks the original UI, capability commands, scenes,
plugin UI, fixed/chunked restores larger than 1 MiB, corrupt-archive rejection and
state after a hard controller restart. It uses only disposable volumes. Evidence
and exact artifact hashes are in `target/qemu-stulp-controller/checks.json`, written
only after both boots pass. The older prebuilt Hop used here can assign two saved
jobs to the same slot during simultaneous cold recovery. The harness removes the
plugin job before the hard stop and starts it again after controller recovery; it
does not establish correctness of simultaneous scheduler recovery. No scheduler
or kernel changes are included in this port.

## Waiting on events, not on the clock

The HopOS runtime follows the contract in HopOS `docs/apps.md`: a slot that
idles yields its core with a wake time, and the kernel divides a shared core
between residents that sleep. Until 2 October 2026 the controller owner woke
every 1 ms and 10 ms, every plugin transport every 5 ms, and the request and
stream workers every 2 to 5 ms, each wake a full round of work. On the
LicheeRV with controller, plugin bundle and tunnel in one sharegroup that
cost the controller 0.2 to 2 s per trivial request and dropped all ten
plugin heartbeats every 8 to 11 minutes.

Now every loop waits on the things that can give it work: `TcpStream::readable()`
and `UdpSocket::readable()` from the SDK (non-consuming, waker based), the
work mailbox, per-worker `Signal`s for answers, stream events and attach
changes, and the reply channel of an SSE stream. Timers remain only as
deadlines and as a floor tick: 50 ms for the controller owner and the plugin
transport (the granularity of heartbeat, Flow, scene and MRP deadlines, the
same measure as the applib heartbeat), 5 ms while a media owner serves
viewers, 1 s as the SSE reader probe. The kernel prints
`slot N: idle=NN% wakes=N/s cores=N HOPOS_SLOT_LOAD` every 30 s for each
slot; on hardware a quiet Stulp slot should sit above 95% idle with wakes in
the tens per second.

### Load on a shared core: who breathes

Sleeping on events fixed the idle case (a trivial request fell from 0.2 to 2 s
to 15 ms on the LicheeRV), but with the house loaded the controller still
stalled 0.4 to 1.5 s, and once 10 s. Stopping the plugin bundle for 40 s
brought the controller to 19 ms with the same house, so the bundle was the
one holding the core: a CASE handshake ran one ephemeral key, one ECDH, one
ECDSA verification and one signature in a single poll, node after node, and
each of its heartbeat timeouts (5 s) tore down all ten plugin attaches, after
which the controller refused the fresh attach for up to 15 s ("already
connected") and every Matter session had to be rebuilt. Three measures:

- The plugin heartbeat deadline is 20 s, longer than the controller's 15 s, so
  the plugin never gives up first on a slow neighbour.
- A fresh attach of an app supersedes its stale connection at once, with the
  same cleanup as a dropped connection.
- The CASE handshake runs in three steps (shared secret, verify, sign) with a
  transport turn between them (`Client::idle`), and the engine waits 250 ms
  between two nodes, so the slot yields its core between the heavy steps.

### Fewer connections, more breathing

Measured with `STULP_LOAD` and `STULP_TLS` on the LicheeRV (2 October,
evening): both slots idle 99% when quiet; during use the bundle fell to 14%
idle with 30 TCP retransmissions per 30 s on its external connections, every
Spotify or TaHoma call cost a full TLS handshake of 160 to 630 ms of
computation, and the UniFi and Matter callbacks ran up to 7 s. Three more
measures:

- The plugin HTTP worker keeps connections alive through leanhttp's pool
  (two per host, eight in total, 30 s), so a poll reuses its connection
  instead of paying a handshake.
- The TLS handshake yields to the executor after the chain verification and
  after the certificate signature, so one handshake no longer runs in one
  breath for the other plugins on the executor.
- `STULP_BUNDLE_SKIP=com.stulp.matter` leaves Matter out of the bundle; the
  node runs it as its own job from `matter-<arch>-tamago.elf` with its own
  executor and heartbeat, so UniFi, Spotify and TaHoma work and Matter work
  no longer queue behind each other.

## Attention model: everyone gets a turn, nobody drags the rest along

Ten plugins share one cooperative executor in the HopOS bundle; HopOS does
not preempt within a slot, and the kernel only rotates slots that yield. On
the LicheeRV every failure mode of October 2026 had the same shape: one plugin
held the executor (a 30 s stall after a Matter handshake, a camera snapshot,
full-document copies per callback), every heartbeat stalled, the controller
dropped all ten attaches, and the reconnect (about 70 inits and dozens of
Matter handshakes) produced the next stall. The model breaks that at four
levels:

1. **Fair turns (SDK).** Every plugin loop goes through `Client::pump`. After
   32 protocol turns it yields the executor unconditionally (`COOP_TURNS`), so
   a loop whose awaits all complete immediately can no longer monopolise the
   bundle. Heavy synchronous steps yield explicitly: the Matter CASE steps,
   the TLS handshake after chain verification and after the signature.
2. **Visible attention (runtime).** Each bundled plugin task is wrapped in a
   meter. A single executor turn of 200 ms or more is logged as
   `STULP_LONG_POLL index= ms=`; `STULP_PLUGIN index= id=` maps indices to
   apps at start; `STULP_LOAD` carries `busy_ms=[index:ms ...]` per 30 s and
   `stall_max_ms`, and a turn more than 1 s late is `STULP_STALL ms=`.
3. **No amplification (protocol).** A missed heartbeat is slow, not dead: the
   plugin logs `STULP_HEARTBEAT_SLOW` and keeps pinging, the controller logs
   `[stulp:app-slow]` and keeps the session. A connection closes on a real
   transport error or after five minutes of complete silence; initialisation
   may take three minutes. A busy minute therefore costs a busy minute, not a
   full restart of every plugin.
4. **Isolation by core (deployment).** On the two-core LicheeRV the controller
   and tunnel share the small core (`sharegroup: hop`) and the bundle has the
   big core (`sharegroup: system`).

Within Matter, maintenance (CASE and subscribe, one node per step) waits until
startup callbacks have been quiet for 3 s, `device.init`/`driver.init` are not
pool barriers, and workers receive a state snapshot only when theirs is stale.

## IPv6 and Thread follow-up

No extra enable flag is needed: Matter opens separate IPv4/IPv6 UDP sockets,
which lazily starts NDP and router solicitation. Link-local addresses use `%1`
inside each single-interface slot. An unknown scope is rejected; multicast and
ULA replies retain the correct scope semantics. The socket bind is `[::]:port`;
Lean selects link-local or its single active SLAAC source per destination.
IPv6 datagrams are capped at 1232 bytes (1280-byte packets without fragmentation).

The port includes bounded neighbor retries, router/default-route expiry,
independent PIO L/A lifetimes, single-address renumbering, RIO replacement and
withdrawal, the renewable two-hour lease cap, cancellation and socket cleanup.
Tests include independent wire fixtures produced by Go v1.2.0.

Physical LicheeRV testing also exposed a Thread router advertising its route
without the optional source link-layer address option and not answering
multicast neighbor solicitations. Lean now learns the Ethernet source of a
fully validated router advertisement even when that option is absent. Invalid
options or hop limits still cannot populate the neighbor cache. The regression
test is `thread_route_without_sllao_learns_validated_ethernet_neighbor` in the
shared Lean checkout; include that fix when building this plugin pack.

`vendor/applib` keeps the alpha.18 SDK plus the IPv6/AAAA additions. Its
network dependency uses the sibling `haas.software/lean/leannet` checkout, just
like the shared HopOS tree. Keep Lean next to Stulp and HopOS when building;
there is only one netstack source tree. `check` validates the SDK snapshot hashes
and runs the SDK and Lean dependency suites. See `vendor/PROVENANCE.md`
before replacing the SDK overlay and local Lean dependency with upstream tags.

`./build.sh qemu-ipv6` starts two disposable HopOS slots and proves NDP,
link-local unicast and `ff02::fb` multicast through the actual switch and SDK.
Set `STULP_QEMU_KERNEL` and `STULP_QEMU_HOP` to existing QEMU-compatible images;
the command never builds or updates them. Evidence is written only after both
peers succeed, in `target/qemu-stulp-ipv6/checks.json`. This is separate from the
controller/UI/cold-storage test (`qemu-controller`). Real Thread hardware has
not been exercised by this port.
