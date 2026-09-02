package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// seedGrantables creates a monitor and a group on the harness's repos so the
// permission endpoints have real ids to accept (and a real id to reject).
func seedGrantables(t *testing.T, h *userHarness) (monitorID, groupID int64) {
	t.Helper()
	ctx := context.Background()

	m := &domain.Monitor{UserID: 1, Name: "m", Type: "http", Active: true, Interval: 60}
	if err := h.monitor.Create(ctx, m); err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	g := &domain.MonitorGroup{UserID: 1, Name: "g", Condition: domain.GroupConditionWorstOfChildren}
	if err := h.groups.Create(ctx, g); err != nil {
		t.Fatalf("create group: %v", err)
	}
	return m.ID, g.ID
}

// GET /api/users/:id/permissions on a user with no grants must return empty
// ARRAYS, not nulls — the admin UI iterates them unconditionally.
func TestUserPermissions_Get_EmptyIsArraysNotNull(t *testing.T) {
	h := newUserHarness(t)
	_, adminToken := h.bootstrapAdmin(t)

	member, err := h.svc.CreateUser(context.Background(), "bob", "supersecret", true, false, "UTC", services.UserCapabilities{})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}

	rec := h.doWithToken(t, http.MethodGet, "/api/users/"+intToStr(member.ID)+"/permissions", nil, adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET permissions = %d; want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"monitor_ids", "groups"} {
		val, ok := raw[key]
		if !ok {
			t.Fatalf("response has no %q field", key)
		}
		if string(val) == "null" {
			t.Errorf("%q serialized as null; must be []", key)
		}
	}
}

// PUT replaces the whole grant set, and the EFFECT is what matters: after the
// PUT the target user must actually be able to see the granted monitor, and after
// a PUT with empty lists they must see nothing. A 200 proves neither.
func TestUserPermissions_Put_ReplacesSetAndChangesVisibility(t *testing.T) {
	h := newUserHarness(t)
	_, adminToken := h.bootstrapAdmin(t)
	ctx := context.Background()

	member, err := h.svc.CreateUser(ctx, "bob", "supersecret", true, false, "UTC", services.UserCapabilities{})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	monitorID, groupID := seedGrantables(t, h)

	// Before: no grants, sees nothing.
	if _, ids, err := h.access.VisibleMonitorIDs(ctx, member.ID); err != nil || len(ids) != 0 {
		t.Fatalf("before the grant the member sees %v (err=%v); want nothing", ids, err)
	}

	// Sent with the DEPRECATED group_ids spelling on purpose: this is the pre-011
	// request shape, and a client still using it must keep working and must keep
	// getting the recursive grant it has always got.
	rec := h.doWithToken(t, http.MethodPut, "/api/users/"+intToStr(member.ID)+"/permissions", map[string]any{
		"monitor_ids": []int64{monitorID},
		"group_ids":   []int64{groupID},
	}, adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT permissions = %d; want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var view struct {
		MonitorIDs []int64 `json:"monitor_ids"`
		Groups     []struct {
			GroupID            int64 `json:"group_id"`
			IncludeDescendants bool  `json:"include_descendants"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(view.MonitorIDs) != 1 || view.MonitorIDs[0] != monitorID {
		t.Errorf("monitor_ids = %v; want [%d]", view.MonitorIDs, monitorID)
	}
	if len(view.Groups) != 1 || view.Groups[0].GroupID != groupID {
		t.Fatalf("groups = %v; want one grant on group %d", view.Groups, groupID)
	}
	if !view.Groups[0].IncludeDescendants {
		t.Error("a legacy group_ids grant came back shallow; it must be deep, or upgrading silently narrows every existing client's grants")
	}

	// The effect: the member can now actually see the monitor.
	all, ids, err := h.access.VisibleMonitorIDs(ctx, member.ID)
	if err != nil {
		t.Fatalf("VisibleMonitorIDs: %v", err)
	}
	if all {
		t.Fatal("granting a monitor made the member an admin — all=true")
	}
	if len(ids) != 1 || ids[0] != monitorID {
		t.Fatalf("after the grant the member sees %v; want [%d] — the PUT returned 200 but granted nothing",
			ids, monitorID)
	}

	// Replace with an empty set: everything is revoked.
	rec = h.doWithToken(t, http.MethodPut, "/api/users/"+intToStr(member.ID)+"/permissions", map[string]any{
		"monitor_ids": []int64{},
		"group_ids":   []int64{},
	}, adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT (revoke all) = %d; want 200", rec.Code)
	}
	if _, ids, err := h.access.VisibleMonitorIDs(ctx, member.ID); err != nil || len(ids) != 0 {
		t.Fatalf("after revoking everything the member still sees %v (err=%v); PUT is merging, not replacing", ids, err)
	}
}

// A grant naming a monitor that does not exist is a 400, and nothing is written.
func TestUserPermissions_Put_RejectsUnknownMonitor(t *testing.T) {
	h := newUserHarness(t)
	_, adminToken := h.bootstrapAdmin(t)
	ctx := context.Background()

	member, err := h.svc.CreateUser(ctx, "bob", "supersecret", true, false, "UTC", services.UserCapabilities{})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	monitorID, _ := seedGrantables(t, h)

	// Seed a good grant first, so we can prove the rejected call did not wipe it.
	if rec := h.doWithToken(t, http.MethodPut, "/api/users/"+intToStr(member.ID)+"/permissions", map[string]any{
		"monitor_ids": []int64{monitorID},
	}, adminToken); rec.Code != http.StatusOK {
		t.Fatalf("seed PUT = %d; want 200", rec.Code)
	}

	rec := h.doWithToken(t, http.MethodPut, "/api/users/"+intToStr(member.ID)+"/permissions", map[string]any{
		"monitor_ids": []int64{monitorID, 99999},
	}, adminToken)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT with an unknown monitor id = %d; want 400", rec.Code)
	}

	if _, ids, err := h.access.VisibleMonitorIDs(ctx, member.ID); err != nil || len(ids) != 1 || ids[0] != monitorID {
		t.Fatalf("the rejected PUT clobbered the existing grants: member sees %v (err=%v); want [%d]",
			ids, err, monitorID)
	}
}

// The capability flags round-trip through POST/PUT /api/users and are reported on
// the user view, so the frontend can hide what the user cannot do.
func TestUserHandlers_CapabilityFlags_RoundTrip(t *testing.T) {
	h := newUserHarness(t)
	_, adminToken := h.bootstrapAdmin(t)

	rec := h.doWithToken(t, http.MethodPost, "/api/users", map[string]any{
		"username":                 "bob",
		"password":                 "supersecret",
		"can_manage_notifications": true,
		"can_view_extensions":      true,
		"can_view_all_monitors":    true,
	}, adminToken)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/users = %d; want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	var created struct {
		User struct {
			ID                     int64 `json:"id"`
			IsAdmin                bool  `json:"is_admin"`
			CanManageNotifications bool  `json:"can_manage_notifications"`
			CanManageMaintenance   bool  `json:"can_manage_maintenance"`
			CanViewExtensions      bool  `json:"can_view_extensions"`
			CanViewAllMonitors     bool  `json:"can_view_all_monitors"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !created.User.CanManageNotifications {
		t.Error("can_manage_notifications was not applied on create")
	}
	if created.User.CanManageMaintenance {
		t.Error("can_manage_maintenance defaulted to true; want false")
	}
	if !created.User.CanViewExtensions {
		t.Error("can_view_extensions was not applied on create")
	}
	if !created.User.CanViewAllMonitors {
		t.Error("can_view_all_monitors was not applied on create")
	}
	if created.User.IsAdmin {
		t.Error("a capability must not imply admin")
	}

	// The service must agree with the wire: capability true, admin false.
	ctx := context.Background()
	if ok, err := h.access.CanManageNotifications(ctx, created.User.ID); err != nil || !ok {
		t.Errorf("CanManageNotifications = (%v, %v); want (true, nil)", ok, err)
	}
	if ok, err := h.access.CanManageMaintenance(ctx, created.User.ID); err != nil || ok {
		t.Errorf("CanManageMaintenance = (%v, %v); want (false, nil)", ok, err)
	}
	if ok, err := h.access.CanViewExtensions(ctx, created.User.ID); err != nil || !ok {
		t.Errorf("CanViewExtensions = (%v, %v); want (true, nil)", ok, err)
	}
	if ok, err := h.access.CanViewAllMonitors(ctx, created.User.ID); err != nil || !ok {
		t.Errorf("CanViewAllMonitors = (%v, %v); want (true, nil)", ok, err)
	}

	// A PUT that omits the flags must LEAVE THEM ALONE, not silently strip them.
	rec = h.doWithToken(t, http.MethodPut, "/api/users/"+intToStr(created.User.ID), map[string]any{
		"username": "bobby",
	}, adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/users/:id = %d; want 200", rec.Code)
	}
	if ok, err := h.access.CanManageNotifications(ctx, created.User.ID); err != nil || !ok {
		t.Errorf("a PUT that omitted can_manage_notifications stripped it: (%v, %v)", ok, err)
	}
	if ok, err := h.access.CanViewExtensions(ctx, created.User.ID); err != nil || !ok {
		t.Errorf("a PUT that omitted can_view_extensions stripped it: (%v, %v)", ok, err)
	}
	if ok, err := h.access.CanViewAllMonitors(ctx, created.User.ID); err != nil || !ok {
		t.Errorf("a PUT that omitted can_view_all_monitors stripped it: (%v, %v)", ok, err)
	}

	// Revoking extension visibility must not disturb an unrelated capability.
	rec = h.doWithToken(t, http.MethodPut, "/api/users/"+intToStr(created.User.ID), map[string]any{
		"can_view_extensions": false,
	}, adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT (revoke extension visibility) = %d; want 200", rec.Code)
	}
	if ok, err := h.access.CanViewExtensions(ctx, created.User.ID); err != nil || ok {
		t.Errorf("CanViewExtensions after an explicit false = (%v, %v); want (false, nil)", ok, err)
	}
	if ok, err := h.access.CanManageNotifications(ctx, created.User.ID); err != nil || !ok {
		t.Errorf("revoking extension visibility disturbed notifications: (%v, %v)", ok, err)
	}
	if ok, err := h.access.CanViewAllMonitors(ctx, created.User.ID); err != nil || !ok {
		t.Errorf("revoking extension visibility disturbed view-all: (%v, %v)", ok, err)
	}

	rec = h.doWithToken(t, http.MethodPut, "/api/users/"+intToStr(created.User.ID), map[string]any{
		"can_view_all_monitors": false,
	}, adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT (revoke view-all) = %d; want 200", rec.Code)
	}
	if ok, err := h.access.CanViewAllMonitors(ctx, created.User.ID); err != nil || ok {
		t.Errorf("CanViewAllMonitors after an explicit false = (%v, %v); want (false, nil)", ok, err)
	}
	if ok, err := h.access.CanManageNotifications(ctx, created.User.ID); err != nil || !ok {
		t.Errorf("revoking view-all disturbed notifications: (%v, %v)", ok, err)
	}

	// And an explicit false revokes notifications too.
	rec = h.doWithToken(t, http.MethodPut, "/api/users/"+intToStr(created.User.ID), map[string]any{
		"can_manage_notifications": false,
	}, adminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT (revoke capability) = %d; want 200", rec.Code)
	}
	if ok, err := h.access.CanManageNotifications(ctx, created.User.ID); err != nil || ok {
		t.Errorf("CanManageNotifications after an explicit false = (%v, %v); want (false, nil)", ok, err)
	}
}
