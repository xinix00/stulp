#!/bin/sh
# Bouwt Stulp en elke app in plugins/*.
#
# Eén app is één map: de binary heet zijn eigen app-id en staat naast app.json,
# want dat is precies waar Stulp hem zoekt (internal/plugin/process.go).
#
#   ./build.sh              alles
#   ./build.sh matter nibe  alleen die apps
#   ./build.sh stulp        alleen de controller
#   ./build.sh hopos        de HopOS-images (ELF per slot, in out/)
#
# GOOS/GOARCH uit de omgeving werken gewoon, dus cross-compilen kan zonder
# hier iets aan te passen: GOOS=linux GOARCH=arm64 ./build.sh
set -e

cd "$(dirname "$0")"

# Eén stempel voor controller en apps. CI/releasebouw kan hem blijven
# overschrijven met STULP_VERSION; een gewone build hoort bij deze bronversie.
stulp_version=${STULP_VERSION:-v0.8.6}

# ---- HopOS ------------------------------------------------------------------
#
# Op een HopOS-node is er geen besturingssysteem onder de binary: geen fork, geen
# bestandssysteem in het proces, geen shell die argumenten meegeeft. Elk
# programma is een ELF dat HOP in een slot plaatst, met een eigen IP.
#
# Dat verandert precies één ding aan Stulp, en dat is hoe apps starten. Stulp kan
# ze daar niet zelf starten (starten is fork+exec), dus is elke app zijn EIGEN
# slot-app die zich over de attach-poort meldt met zijn token -- hetzelfde pad als
# een app in een pod. Zie cmd/stulp/main_tamago.go en
# examples/virtual/start_tamago.go.
#
# Wat er meedoet, bepaalt de boom zelf: elke map met een *_tamago.go bestand.
# Zo verschijnt een plugin in dit doel op het moment dat hij een HopOS-start
# krijgt, en niet doordat iemand deze lijst bijwerkt.
#
# De link is canoniek (HopOS docs/app.md): één artifact draait in élk slot.
# arm64 op SlotBase(1)+0x10000, riscv64 op de fysieke partitie -- daar is geen
# tweede translatiefase.
build_hopos() {
	tamago="${TAMAGO:-$HOME/tamago-go/bin/go}"
	if [ ! -x "$tamago" ]; then
		echo "build: $tamago ontbreekt -- zet TAMAGO=/pad/naar/go" >&2
		exit 1
	fi
	mkdir -p out
	for dir in $(find cmd examples plugins -name '*_tamago.go' -exec dirname {} \; | sort -u); do
		name=$(basename "$dir")
		for arch in riscv64 arm64; do
			case "$arch" in
			# stulp_notls: op het node-netwerk bewijst het token wie er aanklopt,
			# en TLS erbovenop kost een app een hele TLS-stapel voor geheimhouding
			# tegen iets dat al in dat netwerk zit.
			# GEEN -s: HOP's plaatser (leanelf) leest de symboltabel van het
			# image (versiewacht, entry) — met -s weigert élk slot het ELF.
			# -w (DWARF eruit) is de grootte-winst die wél kan.
			riscv64) tags="linkramsize linkcpuinit stulp_notls"; ld="-w -T 0x88010000 -R 0x1000" ;;
			arm64)   tags="linkcpuinit stulp_notls";             ld="-w -T 0x50010000 -R 0x1000" ;;
			esac
			elf="out/$name-$arch-tamago.elf"
			GOWORK=off GOTOOLCHAIN=local GOFLAGS=-mod=mod \
				GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH="$arch" \
				"$tamago" build -tags "$tags" -trimpath \
				-ldflags "$ld -X main.version=$stulp_version -X github.com/xinix00/stulp/internal/appsdk.BuildVersion=$stulp_version" \
				-o "$elf" "./$dir"
			echo "$elf ($(( $(wc -c < "$elf") / 1024 )) kB)"
		done
	done
}

if [ "${1:-}" = "hopos" ]; then
	build_hopos
	exit 0
fi

# -s -w gooit de DWARF- en symbooltabel eruit: scheelt zo'n 30% en doet niets
# tijdens het draaien. Panic-traces houden hun functienamen, want die komen uit
# de pclntab. Laat ze weg als je een debugger wilt aanhaken.
ldflags='-s -w'

# app_id leest de id uit een app.json. Geen jq nodig: het manifest is met de
# hand geschreven en de id staat er altijd als platte string in.
app_id() {
	sed -n 's/^[[:space:]]*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$1" | head -1
}

wants() {
	[ $# -eq 1 ] && return 0 # geen filter opgegeven: alles bouwen
	name=$1
	shift
	for arg in "$@"; do
		[ "$arg" = "$name" ] && return 0
	done
	return 1
}

built=0

if wants stulp "$@"; then
	go build -ldflags="$ldflags -X main.version=$stulp_version" -o stulp ./cmd/stulp
	echo "stulp"
	built=$((built + 1))
fi

for dir in plugins/*/; do
	name=$(basename "$dir")
	[ -f "$dir/app.json" ] || continue
	wants "$name" "$@" || continue

	id=$(app_id "$dir/app.json")
	if [ -z "$id" ]; then
		echo "build: $dir/app.json heeft geen id" >&2
		exit 1
	fi

	go build -ldflags="$ldflags -X github.com/xinix00/stulp/internal/appsdk.BuildVersion=$stulp_version" -o "$dir$id" "./$dir"
	echo "$name -> $dir$id"
	built=$((built + 1))
done

if [ "$built" -eq 0 ]; then
	echo "build: niets gebouwd -- onbekende naam? $*" >&2
	exit 1
fi
