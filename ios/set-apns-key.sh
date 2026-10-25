#!/bin/sh
# Zet Apple's APNs-sleutel in de Meldingen-app van Stulp.
#   ios/set-apns-key.sh ~/Downloads/AuthKey_XXXXXXXXXX.p8 [https://demo.gethop.org]
#
# Leest de .p8 van je schijf en stuurt hem rechtstreeks naar je eigen Stulp, met
# de Manage-sleutel uit configs/stulp.json. De Key ID komt uit de bestandsnaam;
# de Team ID staat in de plugin al op QVY3KR44QS. Er wordt niets gelogd of bewaard.
set -eu
P8=${1:?gebruik: $0 pad/naar/AuthKey_XXXXXXXXXX.p8 [stulp-url]}
BASE=${2:-https://demo.gethop.org}
CONFIGS=${CONFIGS:-$(cd "$(dirname "$0")/../.." && pwd)/configs/stulp.json}

name=$(basename "$P8")
KID=${name#AuthKey_}
KID=${KID%.p8}
case "$KID" in
??????????) ;;
*) echo "set-apns-key: Key ID niet uit '$name' te halen (verwacht AuthKey_<10 tekens>.p8)" >&2; exit 2 ;;
esac

TOKEN=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["env"]["STULP_TOKEN"])' "$CONFIGS")
JAR=$(mktemp)
trap 'rm -f "$JAR"' EXIT
curl -sf -o /dev/null -c "$JAR" "$BASE/$TOKEN"

put() {
	printf '%s: ' "$1"
	curl -sf -b "$JAR" -X PUT -H 'Content-Type: application/json' -d "$2" \
		"$BASE/api/manager/apps/app/com.stulp.notify/setting/$1"
	echo
}
put apnsKey "$(python3 -c 'import json,sys;print(json.dumps({"value":open(sys.argv[1]).read()}))' "$P8")"
put apnsKeyId "{\"value\":\"$KID\"}"
echo "set-apns-key: klaar (Key ID $KID)"
