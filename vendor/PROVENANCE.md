# Source overlay

Stulp builds against tagged releases of HopOS (v3.0.10), Lean (v3.1.9) and Hop
(v3.0.0-alpha.10). One crate is overlaid, with its source hashes in
`SOURCES.json`; `python3 tests/vendor_check.py` verifies them. Licenses are kept
beside the source tree. No crate registry replacement or shared Cargo cache is
edited.

- `types`: Hop v3.0.0-alpha.10 (commit `998ebdc`, the tag Stulp already pins),
  plus `Object::get_mut`, `Object::insert`, `Object::remove` and
  `Value::as_object_mut` in `src/json.rs`, with a test. Without them every
  `json::set` rebuilt the object and deep-copied every other field; one device
  update in a plugin's state copied the whole state (~300 ms on the LicheeRV,
  03-10). The same four methods belong upstream in Hop's `types`; when they are
  there, remove the overlay and its Cargo patch.

Until 04-10 an `applib` overlay (HopOS alpha.18 plus IPv6, AAAA and the exact-class
heap) lived here, with leannet from the sibling Lean checkout. HopOS v3.0.10 ships
all of that itself (leannet v3.1.9 with the `ipv6` feature, the `heap` crate), so
Stulp now uses it directly and no longer needs Lean next to it.
