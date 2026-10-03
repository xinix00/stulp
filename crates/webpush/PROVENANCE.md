# Web Push dependencies

The protocol code ports Stulp's MIT-licensed `internal/webpush` and the existing
browser receiver tests. No Lumen application code is included. The immutable
RustCrypto vendor files were reused from the local Lumen checkout; every crate
retains its own upstream license and Cargo checksum.

P-256 ECDH and ES256 signing are required by Stulp's existing Web Push protocol.
Lean's variable-time signature verifier is not a private-key signing primitive.
The direct dependencies are pinned to p256 0.13.2, aes-gcm 0.10.3, hkdf 0.12.4,
sha2 0.10.9 and zeroize 1.9.0, with default features disabled. There is no OS RNG
in the protocol crate: the SDK supplies fresh entropy. TLS remains in the host
adapter and is verified normally for push services.

`vendor/` also supplies the TLS server signing dependencies documented in
`../transport/PROVENANCE.md`; it contains 73 original registry crates (about 36 MiB), including
conditional target dependencies. `.cargo/config.toml` selects this local source;
Cargo.lock pins the complete set. The build does not fetch registry sources.

The synthetic Rust example `vector` writes one encrypted push message with its
VAPID signature. During the port it was decrypted and verified by an independent
Go implementation; that peer is no longer part of this tree. The example sends
no HTTP request and reads no real keys or subscriptions.