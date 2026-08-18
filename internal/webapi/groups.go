package webapi

import (
	"errors"

	"github.com/xinix00/stulp/internal/stulphttp"

	"github.com/xinix00/stulp/internal/store"
)

func (s *Server) handleDeviceGroups() {
	s.mux.HandleFunc("GET /api/stulp/device-groups", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		groups, err := s.store.DeviceGroups(stulphttp.Context(request))
		if err != nil {
			writeError(response, stulphttp.StatusInternalServerError, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, groups)
	})
	s.mux.HandleFunc("POST /api/stulp/device-groups", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		var group store.DeviceGroup
		if err := decodeJSON(request, &group); err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		created, err := s.store.CreateDeviceGroup(stulphttp.Context(request), group)
		if err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		writeJSON(response, stulphttp.StatusCreated, created)
	})
	s.mux.HandleFunc("PUT /api/stulp/device-groups/{id}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		var body struct {
			Name      *string `json:"name"`
			ParentID  *string `json:"parentId"`
			SortOrder *int    `json:"sortOrder"`
		}
		if err := decodeJSON(request, &body); err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		group, err := s.store.DeviceGroup(stulphttp.Context(request), request.PathValue("id"))
		if err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		if body.Name != nil {
			group.Name = *body.Name
		}
		if body.ParentID != nil && group.ParentID != *body.ParentID {
			group.ParentID = *body.ParentID
			group.SortOrder = 0
		}
		if body.SortOrder != nil {
			group.SortOrder = *body.SortOrder
		}
		updated, err := s.store.UpdateDeviceGroup(stulphttp.Context(request), group)
		if err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, updated)
	})
	s.mux.HandleFunc("DELETE /api/stulp/device-groups/{id}", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		if err := s.store.DeleteDeviceGroup(stulphttp.Context(request), request.PathValue("id")); err != nil {
			writeError(response, stulphttp.StatusNotFound, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, true)
	})
	s.mux.HandleFunc("PUT /api/stulp/devices/{id}/group", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		var body struct {
			GroupID *string `json:"groupId"`
		}
		if err := decodeJSON(request, &body); err != nil || body.GroupID == nil {
			if err == nil {
				err = errors.New("groupId is required")
			}
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		device, err := s.store.SetDeviceGroup(stulphttp.Context(request), request.PathValue("id"), *body.GroupID)
		if err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, s.deviceObject(device))
	})
	s.mux.HandleFunc("PUT /api/stulp/devices/order", func(response stulphttp.ResponseWriter, request *stulphttp.Request) {
		var body struct {
			GroupID   string   `json:"groupId"`
			DeviceIDs []string `json:"deviceIds"`
		}
		if err := decodeJSON(request, &body); err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		if err := s.store.ReorderDevices(stulphttp.Context(request), body.GroupID, body.DeviceIDs); err != nil {
			writeError(response, stulphttp.StatusBadRequest, err)
			return
		}
		writeJSON(response, stulphttp.StatusOK, true)
	})
}
