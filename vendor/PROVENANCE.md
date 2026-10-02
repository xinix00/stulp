# IPv6 SDK source overlay

This directory makes the IPv6 port buildable before upstream release tags exist.
The SDK uses the sibling Lean checkout directly; other dependencies use the
existing pinned git tags or the source trees inside this checkout.
Licenses are retained beside each source tree. No crate registry replacement or
shared Cargo cache is edited.

- `applib`: HopOS v3.0.0-alpha.18 (commit `1c86bec`), plus IPv6/AAAA additions in
  `src/appnet.rs`, `src/appnet/dns.rs` and `src/appnet/tests.rs`. Other runtime
  source, including the allocator, stays at that pinned version. Cargo metadata
  is standalone; sibling SDK dependencies retain alpha.18 git tags.
- `leannet` is not vendored. The SDK depends directly on
  `haas.software/lean/leannet`, using a relative path to the sibling checkout.
  HopOS uses that same source. IPv6, its shared UDP queues, source tests and
  independent wire fixtures are maintained only in Lean.

`SOURCES.json` records every snapshot file's SHA-256 and the exact Go reference
commit. `python3 tests/vendor_check.py` verifies it; the build gate also runs
both dependency test suites. Lean changes are validated by those tests rather
than frozen in a second source snapshot. When replacing this overlay with release tags, keep
the Stulp scope/discovery tests, SDK pump tests, Go wire fixtures and QEMU IPv6
probe green, remove the applib Cargo patch, and regenerate Cargo.lock normally.
No upstream release was published as part of this change.
