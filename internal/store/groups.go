package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// DeviceGroup is a small user-owned tree. A device belongs to zero or one
// group, while a group can contain devices and child groups.
type DeviceGroup struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ParentID  string `json:"parentId,omitempty"`
	SortOrder int    `json:"sortOrder"`
}

func normalizeDeviceGroup(group *DeviceGroup) error {
	group.Name = strings.TrimSpace(group.Name)
	if group.Name == "" {
		return errors.New("group name is required")
	}
	if len(group.Name) > 80 {
		return errors.New("group name is too long")
	}
	if group.SortOrder < 0 {
		return errors.New("group sortOrder cannot be negative")
	}
	group.ParentID = strings.TrimSpace(group.ParentID)
	return nil
}

func (s *Store) CreateDeviceGroup(_ context.Context, group DeviceGroup) (DeviceGroup, error) {
	if err := normalizeDeviceGroup(&group); err != nil {
		return DeviceGroup{}, err
	}
	if group.ID == "" {
		group.ID = newID()
	}
	s.mu.Lock()
	if err := s.validateGroupParentLocked(group.ID, group.ParentID); err != nil {
		s.mu.Unlock()
		return DeviceGroup{}, err
	}
	for _, existing := range s.doc.DeviceGroups {
		if existing.ID == group.ID {
			s.mu.Unlock()
			return DeviceGroup{}, fmt.Errorf("device group %q already exists", group.ID)
		}
		if strings.EqualFold(existing.Name, group.Name) {
			s.mu.Unlock()
			return DeviceGroup{}, fmt.Errorf("a group named %q already exists", group.Name)
		}
	}
	if group.SortOrder == 0 {
		group.SortOrder = s.nextGroupOrderLocked(group.ParentID, "")
	}
	s.doc.DeviceGroups = append(s.doc.DeviceGroups, group)
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return DeviceGroup{}, fmt.Errorf("create device group: %w", err)
	}
	s.publish(Event{Manager: "devices", Type: "group.create", ID: group.ID, Data: group})
	return group, nil
}

func (s *Store) DeviceGroup(_ context.Context, id string) (DeviceGroup, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, group := range s.doc.DeviceGroups {
		if group.ID == id {
			return group, nil
		}
	}
	return DeviceGroup{}, fmt.Errorf("device group %q does not exist", id)
}

func (s *Store) DeviceGroups(_ context.Context) ([]DeviceGroup, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	groups := append([]DeviceGroup(nil), s.doc.DeviceGroups...)
	sort.SliceStable(groups, func(left, right int) bool {
		if groups[left].ParentID != groups[right].ParentID {
			return groups[left].ParentID < groups[right].ParentID
		}
		if groups[left].SortOrder != groups[right].SortOrder {
			return groups[left].SortOrder < groups[right].SortOrder
		}
		if groups[left].Name != groups[right].Name {
			return groups[left].Name < groups[right].Name
		}
		return groups[left].ID < groups[right].ID
	})
	return groups, nil
}

func (s *Store) UpdateDeviceGroup(_ context.Context, group DeviceGroup) (DeviceGroup, error) {
	if group.ID == "" {
		return DeviceGroup{}, errors.New("group id is required")
	}
	if err := normalizeDeviceGroup(&group); err != nil {
		return DeviceGroup{}, err
	}
	s.mu.Lock()
	if err := s.validateGroupParentLocked(group.ID, group.ParentID); err != nil {
		s.mu.Unlock()
		return DeviceGroup{}, err
	}
	index := -1
	for position, existing := range s.doc.DeviceGroups {
		if existing.ID == group.ID {
			index = position
			continue
		}
		if strings.EqualFold(existing.Name, group.Name) {
			s.mu.Unlock()
			return DeviceGroup{}, fmt.Errorf("a group named %q already exists", group.Name)
		}
	}
	if index < 0 {
		s.mu.Unlock()
		return DeviceGroup{}, fmt.Errorf("device group %q does not exist", group.ID)
	}
	if group.SortOrder == 0 {
		group.SortOrder = s.nextGroupOrderLocked(group.ParentID, group.ID)
	}
	s.doc.DeviceGroups[index] = group
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return DeviceGroup{}, fmt.Errorf("update device group %q: %w", group.ID, err)
	}
	s.publish(Event{Manager: "devices", Type: "group.update", ID: group.ID, Data: group})
	return group, nil
}

// DeleteDeviceGroup preserves what the group contained: its devices and child
// groups move up to its parent rather than disappearing with it.
func (s *Store) DeleteDeviceGroup(_ context.Context, id string) error {
	s.mu.Lock()
	index := -1
	for position, group := range s.doc.DeviceGroups {
		if group.ID == id {
			index = position
			break
		}
	}
	if index < 0 {
		s.mu.Unlock()
		return fmt.Errorf("device group %q does not exist", id)
	}
	parentID := s.doc.DeviceGroups[index].ParentID
	nextDeviceOrder := s.nextDeviceOrderLocked(parentID, "")
	movingDevices := make([]int, 0)
	for position, device := range s.doc.Devices {
		if device.GroupID == id {
			movingDevices = append(movingDevices, position)
		}
	}
	sort.SliceStable(movingDevices, func(left, right int) bool {
		leftDevice, rightDevice := s.doc.Devices[movingDevices[left]], s.doc.Devices[movingDevices[right]]
		if leftDevice.SortOrder > 0 && rightDevice.SortOrder > 0 && leftDevice.SortOrder != rightDevice.SortOrder {
			return leftDevice.SortOrder < rightDevice.SortOrder
		}
		return leftDevice.Name < rightDevice.Name
	})
	for _, position := range movingDevices {
		s.doc.Devices[position].GroupID = parentID
		s.doc.Devices[position].SortOrder = nextDeviceOrder
		if nextDeviceOrder > 0 {
			nextDeviceOrder += 10
		}
	}
	for position := range s.doc.DeviceGroups {
		if s.doc.DeviceGroups[position].ParentID == id {
			s.doc.DeviceGroups[position].ParentID = parentID
		}
	}
	s.doc.DeviceGroups = removeWhere(s.doc.DeviceGroups, func(group DeviceGroup) bool { return group.ID == id })
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("delete device group %q: %w", id, err)
	}
	s.publish(Event{Manager: "devices", Type: "group.delete", ID: id})
	return nil
}

func (s *Store) SetDeviceGroup(ctx context.Context, deviceID, groupID string) (Device, error) {
	if groupID != "" {
		if _, err := s.DeviceGroup(ctx, groupID); err != nil {
			return Device{}, err
		}
	}
	s.mu.Lock()
	index := -1
	for position, record := range s.doc.Devices {
		if record.ID == deviceID {
			index = position
			break
		}
	}
	if index < 0 {
		s.mu.Unlock()
		return Device{}, fmt.Errorf("device %q does not exist", deviceID)
	}
	if s.doc.Devices[index].GroupID == groupID {
		device := s.doc.Devices[index].device(cloneMap(s.state[deviceID]))
		s.mu.Unlock()
		return device, nil
	}
	s.doc.Devices[index].GroupID = groupID
	s.doc.Devices[index].SortOrder = s.nextDeviceOrderLocked(groupID, deviceID)
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return Device{}, fmt.Errorf("move device %q: %w", deviceID, err)
	}
	device, err := s.Device(ctx, deviceID)
	if err != nil {
		return Device{}, err
	}
	s.publish(Event{Manager: "devices", Type: "device.update", ID: device.ID, Data: device})
	return device, nil
}

// ReorderDevices replaces the complete order inside one group in a single
// document write. Requiring the complete set keeps a stale browser from
// accidentally dropping a device that was paired or moved in the meantime.
func (s *Store) ReorderDevices(ctx context.Context, groupID string, deviceIDs []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if groupID != "" {
		if _, err := s.DeviceGroup(ctx, groupID); err != nil {
			return err
		}
	}
	s.mu.Lock()
	positions := make(map[string]int)
	for position, device := range s.doc.Devices {
		if device.GroupID == groupID {
			positions[device.ID] = position
		}
	}
	if len(positions) != len(deviceIDs) {
		s.mu.Unlock()
		return errors.New("device order no longer matches the group")
	}
	seen := make(map[string]bool, len(deviceIDs))
	changed := false
	for order, id := range deviceIDs {
		position, exists := positions[id]
		if !exists || seen[id] {
			s.mu.Unlock()
			return fmt.Errorf("device %q is not a unique member of group %q", id, groupID)
		}
		seen[id] = true
		sortOrder := (order + 1) * 10
		if s.doc.Devices[position].SortOrder != sortOrder {
			s.doc.Devices[position].SortOrder = sortOrder
			changed = true
		}
	}
	var err error
	if changed {
		err = s.saveLocked()
	}
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("reorder devices in group %q: %w", groupID, err)
	}
	if changed {
		s.publish(Event{Manager: "devices", Type: "device.reorder", ID: groupID, Data: deviceIDs})
	}
	return nil
}

// nextGroupOrderLocked places a new group after its existing siblings.
func (s *Store) nextGroupOrderLocked(parentID, excludeID string) int {
	highest := 0
	for _, group := range s.doc.DeviceGroups {
		if group.ParentID != parentID || group.ID == excludeID {
			continue
		}
		if group.SortOrder > highest {
			highest = group.SortOrder
		}
	}
	return highest + 1
}

// A zero order belongs to documents from before tile ordering existed. As long
// as such devices remain together, keep their familiar alphabetical order. The
// first explicit reorder assigns every sibling a positive order at once.
func (s *Store) nextDeviceOrderLocked(groupID, excludeID string) int {
	highest := 0
	found := false
	for _, device := range s.doc.Devices {
		if device.GroupID != groupID || device.ID == excludeID {
			continue
		}
		found = true
		if device.SortOrder == 0 {
			return 0
		}
		if device.SortOrder > highest {
			highest = device.SortOrder
		}
	}
	if !found {
		return 0
	}
	return highest + 10
}

// validateGroupParentLocked refuses a parent that does not exist, and refuses
// to make a group its own ancestor.
func (s *Store) validateGroupParentLocked(groupID, parentID string) error {
	if parentID == "" {
		return nil
	}
	if parentID == groupID {
		return errors.New("a group cannot contain itself")
	}
	visited := map[string]bool{groupID: true}
	for current := parentID; current != ""; {
		if visited[current] {
			return errors.New("group parent would create a cycle")
		}
		visited[current] = true
		found := false
		for _, group := range s.doc.DeviceGroups {
			if group.ID != current {
				continue
			}
			found, current = true, group.ParentID
			break
		}
		if !found {
			return fmt.Errorf("parent device group %q does not exist", current)
		}
	}
	return nil
}
