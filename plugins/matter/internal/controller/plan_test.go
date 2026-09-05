package controller

import (
	"testing"

	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

func testLamp() Device {
	capabilityEndpoints := map[string]uint16{"onoff": 1, "dim": 1, "light_hue": 1, "light_saturation": 1}
	return Device{
		ID: "lamp", DriverID: "matter", Class: "light",
		Capabilities: []string{"onoff", "dim", "light_hue", "light_saturation"},
		Store:        testMatterStore(1, capabilityEndpoints, onOffCluster, levelCluster, colorControlCluster),
	}
}

func commandIDs(planned []plannedCommand) [][2]uint32 {
	ids := make([][2]uint32, 0, len(planned))
	for _, plan := range planned {
		ids = append(ids, [2]uint32{plan.command.Path.Cluster, plan.command.Path.Command})
	}
	return ids
}

// A scene's bundle becomes as few Matter commands as the clusters allow, in an
// order the lamp accepts: power on before the rest, power off after it.
func TestPlanCommandsCombinesWhatTheClustersCombine(t *testing.T) {
	lamp := testLamp()

	planned, failed := planCommands(lamp, 1, map[string]any{"onoff": true, "dim": 0.5, "light_hue": 0.2, "light_saturation": 0.9})
	if len(failed) != 0 {
		t.Fatalf("plan failures = %v", failed)
	}
	want := [][2]uint32{{levelCluster, 4}, {colorControlCluster, 6}}
	if got := commandIDs(planned); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("commands = %v, want MoveToLevelWithOnOff then MoveToHueAndSaturation %v", got, want)
	}
	if len(planned[0].capabilities) != 2 || planned[0].capabilities[0] != "onoff" || planned[0].capabilities[1] != "dim" ||
		len(planned[1].capabilities) != 2 || planned[1].capabilities[0] != "light_hue" || planned[1].capabilities[1] != "light_saturation" {
		t.Fatalf("command coverage = %#v / %#v", planned[0].capabilities, planned[1].capabilities)
	}

	// Off plus a level stays two commands, level first: MoveToLevelWithOnOff
	// at level 0 would turn the lamp off, and a level after the off wakes it.
	planned, failed = planCommands(lamp, 1, map[string]any{"onoff": false, "dim": 0.3})
	if len(failed) != 0 {
		t.Fatalf("plan failures = %v", failed)
	}
	want = [][2]uint32{{levelCluster, 4}, {onOffCluster, 0}}
	if got := commandIDs(planned); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("off + level commands = %v, want %v", got, want)
	}

	// On plus level 0 is not combined either: that would switch the lamp off.
	planned, _ = planCommands(lamp, 1, map[string]any{"onoff": true, "dim": 0.0})
	want = [][2]uint32{{onOffCluster, 1}, {levelCluster, 4}}
	if got := commandIDs(planned); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("on + level 0 commands = %v, want %v", got, want)
	}

	// Hue alone stays MoveToHue; a capability no mapping can send is reported
	// and the rest still goes out.
	planned, failed = planCommands(lamp, 1, map[string]any{"light_hue": 0.2, "target_temperature": 21.0})
	if got := commandIDs(planned); len(got) != 1 || got[0] != [2]uint32{colorControlCluster, 0} {
		t.Fatalf("hue-only commands = %v", got)
	}
	if failed["target_temperature"] == nil || len(failed) != 1 {
		t.Fatalf("unsupported capability was not reported: %v", failed)
	}
}

func TestMoveToHueAndSaturationEncodesBothValues(t *testing.T) {
	command, timed, err := hueAndSaturationCommand(2, 0.5, 1.0)
	if err != nil || timed || command.Path.Endpoint != 2 || command.Path.Cluster != colorControlCluster || command.Path.Command != 0x06 {
		t.Fatalf("command = %#v timed=%v err=%v", command.Path, timed, err)
	}
	payload, err := im.EncodeInvokeRequest([]im.Command{command}, false)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := im.DecodeInvokeRequest(payload)
	if err != nil || len(decoded) != 1 || len(decoded[0].Fields.Children) != 5 {
		t.Fatalf("decoded command = %#v, %v", decoded, err)
	}
	hue, _ := decoded[0].Fields.Field(0)
	saturation, _ := decoded[0].Fields.Field(1)
	transition, _ := decoded[0].Fields.Field(2)
	if hue.Type != tlv.TypeUint || hue.Uint != 127 || saturation.Uint != 254 || transition.Uint != 0 {
		t.Fatalf("hue/saturation/transition = %#v/%#v/%#v, want 127/254/0", hue, saturation, transition)
	}
	if _, _, err := hueAndSaturationCommand(2, 0.5, 1.5); err == nil {
		t.Fatal("saturation above 1 was accepted")
	}
}
