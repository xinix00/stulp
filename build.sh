#!/bin/sh
# Bouwt Stulp: de controller (stulp-host) en elke app in plugins/*.
#
#   ./build.sh            native release-binaries in target/release/
#   ./build.sh linux      statische linux-binaries voor arm64 en riscv64 in out/
#   ./build.sh hopos      HopOS-slot-ELF's per arch in out/ (gestript, laadbaar)
#   ./build.sh check      formattering, clippy, tests, procestests en no_std-checks
#   ./build.sh miri       de platformlaag onder Miri (nightly)
#   ./build.sh qemu-persist | qemu-controller | qemu-ipv6
#
# De versie komt uit Cargo.toml ([workspace.package].version); tools/release.sh
# controleert dat die overeenkomt met de release die hij publiceert.
# Raakt geen HopOS-bronnen en geen huisconfig aan; tests gebruiken wegwerpstaat.
set -eu
stulp_root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$stulp_root"

plugins="virtualdevices weather somfy nibe spotify notify wiim sigenergy unifi matter"

# De objcopy uit de eigen toolchain: geen binutils nodig op de host.
objcopy() {
    set -- "$(rustc --print sysroot)"/lib/rustlib/*/bin/rust-objcopy
    [ -x "$1" ] || { echo 'build: rust-objcopy ontbreekt (rustup component add llvm-tools)' >&2; exit 1; }
    echo "$1"
}

# HopOS-ELF's: alles eruit behalve de vier symbolen die de plaatser van HOP
# patcht. Alleen debuginfo strippen laat een symbooltabel over die het geheugen
# van de LicheeRV-loader opmaakte.
package_hopos() {
    "$(objcopy)" --strip-all \
        --keep-symbol=runtime/goos.RamStart \
        --keep-symbol=runtime/goos.RamSize \
        --keep-symbol=github.com/xinix00/HopOS/metal/v2/board/hopslot.slotHint \
        --keep-symbol=github.com/xinix00/HopOS/metal/v2/app/applib.abiVersion \
        "$1" "$2"
    echo "$2 ($(( $(wc -c < "$2") / 1024 )) kB)"
}

build_linux() {
    mkdir -p out
    for stulp_arch in arm64 riscv64; do
        case "$stulp_arch" in
            arm64) stulp_target=aarch64-unknown-linux-musl ;;
            riscv64) stulp_target=riscv64gc-unknown-linux-musl ;;
        esac
        cargo build --locked --release --workspace --exclude stulp-persist-check --exclude stulp-hopos-app --exclude stulp-hopos-plugins --bins --target "$stulp_target"
        "$(objcopy)" --strip-all "target/$stulp_target/release/stulp-host" "out/stulp-linux-$stulp_arch"
        echo "out/stulp-linux-$stulp_arch"
        for stulp_plugin in $plugins; do
            "$(objcopy)" --strip-all "target/$stulp_target/release/stulp-$stulp_plugin" "out/$stulp_plugin-linux-$stulp_arch"
            echo "out/$stulp_plugin-linux-$stulp_arch"
        done
    done
}

# De namen zijn die van de startup-files op de nodes: stulp-<arch>-tamago.elf,
# <app>-<arch>-tamago.elf en all-plugins-<arch>-tamago.elf. Het achtervoegsel
# is historisch; de URL op de rollende release is wat telt.
build_hopos() {
    mkdir -p out
    for stulp_arch in arm64 riscv64; do
        case "$stulp_arch" in
            arm64) stulp_target=aarch64-unknown-none-softfloat ;;
            riscv64) stulp_target=riscv64gc-unknown-none-elf ;;
        esac
        cargo build --locked --release -p stulp-hopos-app -p stulp-hopos-plugins --bins --target "$stulp_target"
        stulp_out="target/$stulp_target/release"
        package_hopos "$stulp_out/stulp-hopos-app" "out/stulp-$stulp_arch-tamago.elf"
        package_hopos "$stulp_out/stulp-all-plugins-hopos" "out/all-plugins-$stulp_arch-tamago.elf"
        for stulp_plugin in $plugins; do
            package_hopos "$stulp_out/stulp-$stulp_plugin-hopos" "out/$stulp_plugin-$stulp_arch-tamago.elf"
        done
    done
}

case "${1:-build}" in
    build)
        exec cargo build --locked --release --workspace --exclude stulp-persist-check --exclude stulp-hopos-app --exclude stulp-hopos-plugins --bins
        ;;
    linux)
        build_linux
        ;;
    hopos)
        build_hopos
        ;;
    check)
        python3 tests/vendor_check.py
        cargo fmt --all --check
        cargo clippy --locked --workspace --all-targets -- -D warnings
        cargo test --locked --workspace
        cargo build --locked -p stulp-host -p stulp-virtualdevices -p stulp-somfy -p stulp-unifi
        python3 tests/plugins.py
        python3 tests/media.py
        for stulp_target in aarch64-unknown-none-softfloat riscv64gc-unknown-none-elf; do
            cargo check --locked -p stulp-hopos -p stulp-controller -p stulp-core -p stulp-protocol -p stulp-runtime -p stulp-web -p stulp-sdk -p stulp-virtualdevices -p stulp-weather -p stulp-somfy -p stulp-nibe -p stulp-spotify -p stulp-notify -p stulp-wiim -p stulp-sigenergy -p stulp-unifi -p stulp-matter -p stulp-webpush --no-default-features --lib --target "$stulp_target"
            cargo build --locked --release -p stulp-persist-check --bins --target "$stulp_target"
        done
        build_hopos
        ;;
    qemu-ipv6)
        exec python3 tests/qemu_ipv6.py
        ;;
    qemu-controller)
        exec python3 tests/qemu_controller.py
        ;;
    qemu-persist)
        exec python3 tests/qemu_persist.py
        ;;
    miri)
        # De nightly-sysroot heeft eigen dependencies; bouw die buiten onze vendormap.
        (cd /tmp && cargo +nightly miri setup)
        cargo +nightly miri test --locked -p stulp-platform
        (cd /tmp && cargo +nightly miri setup --target aarch64-unknown-linux-gnu)
        cargo +nightly miri test --locked -p stulp-platform --target aarch64-unknown-linux-gnu
        ;;
    *)
        echo 'usage: ./build.sh [build|linux|hopos|check|miri|qemu-persist|qemu-controller|qemu-ipv6]' >&2
        exit 2
        ;;
esac
