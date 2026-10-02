`zoneinfo.zip` is copied unchanged from Go 1.26.4's `lib/time/zoneinfo.zip`.
It contains the IANA TZif data used by the original Go implementation. The
runtime decodes it with Stulp's bounded ZIP/TZif readers; Go is not needed to
build or run the Rust images. IANA zone data is public domain; Go's packaging
license is retained in GO-LICENSE. A mounted `/data/zoneinfo/<name>` overrides
this snapshot on HopOS.
