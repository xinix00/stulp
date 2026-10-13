#!/bin/sh
# Bouwt een gesigneerde Stulp.ipa voor een eigen iPhone of iPad.
#
#   ./build-ipa.sh                 archiveert en exporteert naar build/Stulp.ipa
#   ./build-ipa.sh install [id]    bouwt tegen het gekoppelde toestel en installeert
#
# Development-signing via het team in project.yml; Xcode registreert zelf de
# App IDs, de app group en de Matter-entitlement. Een toestel komt alleen in
# het profiel wanneer het de bouwbestemming is, dus bij install archiveren we
# tegen de iPhone zelf. Vereist het Apple-account van dat team in Xcode, een
# gekoppelde iPhone (kabel, 'Vertrouw') met Ontwikkelaarsmodus aan.
set -eu
set -o pipefail
cd "$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
mkdir -p build

stulp_destination='generic/platform=iOS'
stulp_device=
if [ "${1:-}" = install ]; then
    xcrun devicectl list devices --json-output build/devices.json >/dev/null
    stulp_device=$(python3 - build/devices.json "${2:-}" <<'PY'
import json, sys
devices = json.load(open(sys.argv[1]))["result"]["devices"]
wanted = sys.argv[2]
ready = [d for d in devices
         if (wanted and wanted in (d["identifier"], d["hardwareProperties"].get("udid"), d["deviceProperties"].get("name")))
         or (not wanted and d["connectionProperties"].get("pairingState") == "paired"
             and d["connectionProperties"].get("tunnelState") != "unavailable")]
if len(ready) != 1:
    print("build-ipa: geen (of meer dan een) gekoppeld en bereikbaar toestel:", file=sys.stderr)
    for d in devices:
        print("  %s  %s  pairing=%s  tunnel=%s" % (
            d["identifier"], d["deviceProperties"].get("name"),
            d["connectionProperties"].get("pairingState"),
            d["connectionProperties"].get("tunnelState")), file=sys.stderr)
    print("  sluit de iPhone met een kabel aan, ontgrendel hem en tik op 'Vertrouw';"
          " of geef het identifier mee: ./build-ipa.sh install <id>", file=sys.stderr)
    sys.exit(1)
print(ready[0]["identifier"], ready[0]["hardwareProperties"]["udid"])
PY
    )
    stulp_udid=${stulp_device#* }
    stulp_device=${stulp_device%% *}
    stulp_destination="platform=iOS,id=$stulp_udid"
fi

xcodegen generate
if [ -n "$stulp_device" ]; then
    # Alleen een gewone build tegen het toestel zet zijn UDID in het profiel;
    # de archive-actie registreert niets en valt stil terug op 'generic'.
    xcodebuild -project Stulp.xcodeproj -scheme Stulp -configuration Release \
        -destination "$stulp_destination" \
        -allowProvisioningUpdates -allowProvisioningDeviceRegistration \
        -derivedDataPath build/DerivedData build | grep -E "error|warning: .*(sign|provision)|\*\* "
fi
xcodebuild -project Stulp.xcodeproj -scheme Stulp -configuration Release \
    -destination 'generic/platform=iOS' \
    -allowProvisioningUpdates \
    -archivePath build/Stulp.xcarchive archive | grep -E "error|warning: .*(sign|provision)|\*\* "
xcodebuild -exportArchive -archivePath build/Stulp.xcarchive \
    -exportOptionsPlist ExportOptions.plist -exportPath build \
    -allowProvisioningUpdates | grep -E "error|EXPORT"
echo "build/Stulp.ipa ($(( $(wc -c < build/Stulp.ipa) / 1024 )) kB)"

if [ -n "$stulp_device" ]; then
    xcrun devicectl device install app --device "$stulp_device" build/Stulp.ipa
fi
