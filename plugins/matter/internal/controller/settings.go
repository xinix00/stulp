package controller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/xinix00/stulp/plugins/matter/internal/im"
	"github.com/xinix00/stulp/plugins/matter/internal/tlv"
)

const sensitivityFeature uint64 = 1 << 3

// MatterSetting describes a writable attribute discovered from the
// accessory's own AttributeList and FeatureMap. The UI renders this metadata;
// it never needs a product-specific Aqara table.
type MatterSetting struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Endpoint  uint16 `json:"endpoint"`
	Cluster   string `json:"cluster"`
	Attribute string `json:"attribute"`
	Levels    uint8  `json:"levels,omitempty"`
}

func SettingsForDevice(device Device) []MatterSetting {
	return storedMatterSettings(device.Store["matter.settings"])
}

func inspectEndpointSettings(ctx context.Context, client attributeClient, endpoint uint16,
	inventory *EndpointInventory) (map[string]any, []MatterSetting) {
	settings := make(map[string]any)
	knownCurrent, hasCurrent := inventoryHasAttribute(*inventory, booleanStateConfigurationCluster, 0x0000)
	knownLevels, hasLevels := inventoryHasAttribute(*inventory, booleanStateConfigurationCluster, 0x0001)
	if !knownCurrent || !knownLevels || !hasCurrent || !hasLevels || !inventoryHasSensitivityFeature(*inventory) {
		return settings, nil
	}
	current, _, currentErr := readAttribute(ctx, client, endpoint, booleanStateConfigurationCluster, 0x0000)
	levels, _, levelsErr := readAttribute(ctx, client, endpoint, booleanStateConfigurationCluster, 0x0001)
	if currentErr != nil || levelsErr != nil {
		addInventoryError(inventory, booleanStateConfigurationCluster,
			fmt.Sprintf("read sensitivity configuration: %v", errors.Join(currentErr, levelsErr)))
		return settings, nil
	}
	if current.Type != tlv.TypeUint || levels.Type != tlv.TypeUint || levels.Uint < 2 || levels.Uint > math.MaxUint8 || current.Uint >= levels.Uint {
		addInventoryError(inventory, booleanStateConfigurationCluster, "sensitivity attributes have invalid values")
		return settings, nil
	}
	id := fmt.Sprintf("matter.sensitivity.%d", endpoint)
	settings[id] = current.Uint
	return settings, []MatterSetting{{
		ID: id, Kind: "sensitivity", Endpoint: endpoint,
		Cluster: matterHexID(booleanStateConfigurationCluster), Attribute: matterHexID(0), Levels: uint8(levels.Uint),
	}}
}

func inventoryHasSensitivityFeature(inventory EndpointInventory) bool {
	for _, cluster := range inventory.Clusters {
		if cluster.ID != matterHexID(booleanStateConfigurationCluster) {
			continue
		}
		if cluster.FeatureMap == "" {
			// AttributeList still provides definitive support when a metadata read
			// was lost on a sleepy accessory.
			return true
		}
		featureMap, err := strconv.ParseUint(strings.TrimPrefix(cluster.FeatureMap, "0x"), 16, 64)
		return err == nil && featureMap&sensitivityFeature != 0
	}
	return false
}

func addInventoryError(inventory *EndpointInventory, clusterID uint32, message string) {
	for index := range inventory.Clusters {
		if inventory.Clusters[index].ID == matterHexID(clusterID) {
			inventory.Clusters[index].Errors = append(inventory.Clusters[index].Errors, message)
			return
		}
	}
}

func storedMatterSettings(raw any) []MatterSetting {
	if values, ok := raw.([]MatterSetting); ok {
		return append([]MatterSetting(nil), values...)
	}
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	result := make([]MatterSetting, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		endpoint, endpointOK := number(object["endpoint"])
		levels, levelsOK := number(object["levels"])
		setting := MatterSetting{
			ID: inventoryString(object["id"]), Kind: inventoryString(object["kind"]),
			Cluster: inventoryString(object["cluster"]), Attribute: inventoryString(object["attribute"]),
		}
		if !endpointOK || endpoint < 0 || endpoint > math.MaxUint16 || !levelsOK || levels < 0 || levels > math.MaxUint8 || setting.ID == "" {
			continue
		}
		setting.Endpoint, setting.Levels = uint16(endpoint), uint8(levels)
		result = append(result, setting)
	}
	return result
}

func mergeMatterSettings(left, right any) []MatterSetting {
	byID := make(map[string]MatterSetting)
	for _, setting := range append(storedMatterSettings(left), storedMatterSettings(right)...) {
		byID[setting.ID] = setting
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	result := make([]MatterSetting, 0, len(ids))
	for _, id := range ids {
		result = append(result, byID[id])
	}
	return result
}

// UpdateDeviceSettings validates and writes settings through Matter's generic
// Write interaction before changing Stulp's local copy.
func (c *Controller) UpdateDeviceSettings(ctx context.Context, deviceID string, patch map[string]any) (Device, error) {
	device, err := c.store.Device(ctx, deviceID)
	if err != nil {
		return Device{}, err
	}
	available := make(map[string]MatterSetting)
	for _, setting := range storedMatterSettings(device.Store["matter.settings"]) {
		available[setting.ID] = setting
	}
	ids := make([]string, 0, len(patch))
	values := make(map[string]uint8, len(patch))
	for id, raw := range patch {
		setting, ok := available[id]
		if !ok {
			return Device{}, fmt.Errorf("Matter setting %q is not supported by this device", id)
		}
		value, ok := settingLevel(raw)
		if !ok || value >= setting.Levels {
			return Device{}, fmt.Errorf("%s needs a sensitivity level from 0 through %d", id, setting.Levels-1)
		}
		ids, values[id] = append(ids, id), value
	}
	slices.Sort(ids)
	if len(ids) == 0 {
		return device, nil
	}
	info, err := deviceConnection(device)
	if err != nil {
		return Device{}, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		session, sessionErr := c.session(ctx, info)
		if sessionErr != nil {
			return Device{}, sessionErr
		}
		client := im.Client{Transport: c.node, Session: session}
		err = nil
		for _, id := range ids {
			if writeErr := writeMatterSetting(ctx, client, available[id], values[id]); writeErr != nil {
				err = writeErr
				break
			}
		}
		if err == nil {
			c.reportMu.Lock()
			latest, readErr := c.store.Device(ctx, deviceID)
			if readErr == nil {
				if latest.Settings == nil {
					latest.Settings = make(map[string]any)
				}
				for _, id := range ids {
					latest.Settings[id] = values[id]
				}
				readErr = c.store.UpdateDevice(ctx, latest)
			}
			c.reportMu.Unlock()
			return latest, readErr
		}
		c.expireSession(info.nodeID, session)
	}
	return Device{}, err
}

func settingLevel(raw any) (uint8, bool) {
	if text, ok := raw.(string); ok {
		value, err := strconv.ParseUint(strings.TrimSpace(text), 10, 8)
		return uint8(value), err == nil
	}
	value, ok := number(raw)
	if !ok || value < 0 || value > math.MaxUint8 || math.Trunc(value) != value {
		return 0, false
	}
	return uint8(value), true
}

type attributeWriter interface {
	Write(context.Context, ...im.AttributeWrite) ([]im.AttributeWriteResult, error)
}

func writeMatterSetting(ctx context.Context, client attributeWriter, setting MatterSetting, value uint8) error {
	if setting.Kind != "sensitivity" {
		return fmt.Errorf("Matter setting kind %q is not writable", setting.Kind)
	}
	results, err := client.Write(ctx, im.AttributeWrite{
		Path:  im.ConcreteAttributePath(setting.Endpoint, booleanStateConfigurationCluster, 0),
		Value: func(writer *tlv.Writer, tag tlv.Tag) { writer.PutUintWidth(tag, uint64(value), 1) },
	})
	if err != nil {
		return err
	}
	if len(results) != 1 || !results[0].Status.OK() {
		return errors.New("Matter sensitivity write was not accepted")
	}
	return nil
}
