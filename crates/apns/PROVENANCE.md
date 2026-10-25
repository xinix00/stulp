# APNs and vendored dependencies

The protocol code builds Apple Push Notification service requests for the Stulp
iOS app: the key from Apple's `.p8`, the ES256 provider token, the JSON payload
and the HTTP/2 request. Sending is the plugin's `Client::http`; the HTTP/2
transport is `crates/h2`. This crate used to hold Web Push (VAPID, RFC 8291);
that was removed on 25-10, when notifications moved to the app only.

ES256 signing uses p256 0.13.2 (`ecdsa`, default features disabled) and zeroize
1.9.0. Lean's variable-time signature verifier is not a private-key signing
primitive. There is no OS RNG in the crate: ECDSA nonces are deterministic
(RFC 6979).

`vendor/` also supplies the TLS server signing dependencies documented in
`../transport/PROVENANCE.md`; it contains the original registry crates with their
upstream licenses and Cargo checksums, including conditional target
dependencies and crates the Web Push code used to need. `.cargo/config.toml`
selects this local source; Cargo.lock pins the complete set. The build does not
fetch registry sources.
