# Stulp transport: TLS server key

The TLS 1.3 stack (record layer, key schedule, client, certificate checks and
the server handshake) is Lean's `leantls`, used as a dependency; Stulp no longer
carries a copy. `leantls::server` leaves the private key with the caller through
its `Identity` trait, and `src/key.rs` is that caller.

`KeyPair` loads the usual Go PEM keys: PKCS#8, EC SEC1 and RSA PKCS#1. Signing
uses RustCrypto p256 0.13.2, p384 0.13.1, ed25519-dalek 2.2.0 and rsa 0.9.10,
with default features disabled. RSA is bounded to 2048-4096 bits and uses blinded
PSS/SHA-256 with a fresh OS-seeded ChaCha20 RNG (rand_chacha 0.3.1), never
private-key operations in Lean's variable-time public verifiers. No RSA key
generation is exposed. `KeyPair` also offers P-256 ECDH (through
`Identity::agree_p256`) for clients without X25519. All key types wipe their
private parts on Drop, and `KeyPair::new` checks that the key matches the leaf
certificate before anything listens.

The pinned registry sources and their transitive dependencies are vendored under
`../webpush/vendor`, with upstream license files and Cargo checksums.

Every connection has one owner, without Arc, a mutex or a new executor. The host
adapter bounds attach connections to 32 and HTTP workers to eight. It borrows the
immutable identity; no private key is reparsed or copied per connection. TLS has
an absolute ten-second handshake budget and a 250 ms close_notify budget.
Identity files are explicitly configured, bounded to 1 MiB, and key buffers are
wiped.

The server negotiates TLS 1.3, AES-128-GCM/SHA-256, X25519 or P-256, and optional
HTTP/1.1 ALPN. TLS 1.2, P-521, HelloRetryRequest, client certificates, tickets,
PSK and 0-RTT are not implemented. These limits differ from Go's general TLS
stack and must not be described as full TLS-stack parity.

An independent Go TLS client (`testdata/goclient`) tests all four signature
families, both ECDH groups, legacy key formats, mismatched keys, wrong
certificate names, incompatible ALPN/TLS versions, record-spanning traffic and
authenticated closure. Lean's own tests cover forged Finished, premature
application bytes and corrupted ciphertext.
