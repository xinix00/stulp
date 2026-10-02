# Stulp TLS extension

The record layer, key schedule, cryptographic primitives, client, certificate
verification and original tests are copied from Lean v3.1.1, commit
`db6724745a2c579382c54a68c73c18d643d40038` (MIT; original license in `LEAN-LICENSE`). No Lean
working-tree files are modified. Lean exposes only a client; its private
record/schedule implementation cannot be extended across a crate boundary.

Stulp adds the server handshake in `src/server.rs`, role-specific post-handshake
validation, cancellation-safe ephemeral key wiping, and IP SAN verification
(without DNS/CommonName fallback). Existing signature and chain checks remain.
Private signing uses RustCrypto p256 0.13.2, p384 0.13.1, ed25519-dalek 2.2.0 and
rsa 0.9.10, with default features disabled. RSA uses blinded PSS/SHA-256 and a
fresh OS-seeded ChaCha20 RNG (rand_chacha 0.3.1), never private-key operations in
Lean's variable-time public verifiers. No RSA key generation is exposed; the
upstream bignum prime-generation caches are outside this execution path.

Their pinned registry sources and transitive dependencies are vendored under
`../webpush/vendor`, retaining upstream license files and Cargo checksums. This
shared vendor directory now contains 73 crates (about 36 MiB on this checkout).
Cargo.lock pins optional/target-specific packages too; not all are compiled.

Every connection has one owner, without Arc, a mutex or a new executor. The
inherited record buffers reserve approximately 49 KiB per connection. The host
adapter bounds attach connections to 32 and HTTP workers to eight. It borrows the
immutable identity; no private key is reparsed/copied per accepted connection.
TLS has an absolute ten-second handshake budget and a 250 ms close_notify budget.
Identity files are explicitly configured, bounded to 1 MiB, and key buffers are
wiped. The certificate/private-key match is checked before publishing listeners.

The server negotiates TLS 1.3, AES-128-GCM/SHA-256, X25519 or P-256, and optional
HTTP/1.1 ALPN. PKCS#8, EC SEC1 and RSA PKCS#1 keys are supported; RSA is bounded
to 2048-4096 bits. TLS 1.2, P-521, HelloRetryRequest, client certificates, tickets,
PSK and 0-RTT are not implemented. These limits differ from Go's general TLS
stack and must not be described as full TLS-stack parity.

An independent Go TLS client tests all four signature families, both ECDH groups,
legacy key formats, wrong certificate names, incompatible ALPN/TLS versions,
record-spanning traffic and authenticated closure. Forged Finished, premature
application bytes and corrupted ciphertext fail before exposing a connection.
The native integration tests cover verified-IP TLS attach in both Go/Rust
directions, original HTTP routes, live events, camera video/snapshots and large
streaming restore over HTTPS. ARM64 and RISC-V no_std builds are checked by
`build-rust.sh check`; native transport itself remains in `stulp-transport`.
