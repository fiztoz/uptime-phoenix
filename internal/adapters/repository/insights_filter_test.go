package repository_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestInsightsFilterContract(t *testing.T) {
	for name, factory := range map[string]repositoryFactory{"sqlite": sqliteFactory, "mariadb": mariadbFactory} {
		t.Run(name, func(t *testing.T) {
			repos := factory(t)
			ctx := context.Background()
			user := createUser(t, ctx, repos, "insights-filter")
			parent := &domain.MonitorGroup{UserID: user.ID, Name: "Parent", Condition: domain.GroupConditionWorstOfChildren}
			if err := repos.monitorGroups.Create(ctx, parent); err != nil {
				t.Fatal(err)
			}
			child := &domain.MonitorGroup{UserID: user.ID, Name: "Child", ParentID: &parent.ID, Condition: domain.GroupConditionWorstOfChildren}
			if err := repos.monitorGroups.Create(ctx, child); err != nil {
				t.Fatal(err)
			}
			root := createMonitor(t, ctx, repos, user.ID, "A parent")
			root.GroupID = &parent.ID
			nested := createMonitor(t, ctx, repos, user.ID, "B nested")
			nested.GroupID = &child.ID
			for _, m := range []*domain.Monitor{root, nested} {
				if err := repos.monitors.Update(ctx, m); err != nil {
					t.Fatal(err)
				}
			}
			bare := createMonitor(t, ctx, repos, user.ID, "C ungrouped")
			for _, tc := range []struct {
				name   string
				filter ports.MonitorFilter
				want   []int64
			}{
				{"descendants", ports.MonitorFilter{RestrictToGroupIDs: true, GroupIDs: []int64{parent.ID, child.ID}}, []int64{root.ID, nested.ID}},
				{"empty-groups", ports.MonitorFilter{RestrictToGroupIDs: true}, []int64{}},
				{"empty-grants", ports.MonitorFilter{RestrictToGroupIDs: true, GroupIDs: []int64{parent.ID, child.ID}, RestrictToIDs: true}, []int64{}},
				{"intersect-grants", ports.MonitorFilter{RestrictToGroupIDs: true, GroupIDs: []int64{parent.ID, child.ID}, RestrictToIDs: true, MonitorIDs: []int64{nested.ID, bare.ID}}, []int64{nested.ID}},
				{"intersect-single-group", ports.MonitorFilter{RestrictToGroupIDs: true, GroupIDs: []int64{parent.ID, child.ID}, GroupID: &child.ID}, []int64{nested.ID}},
				{"intersect-ungrouped", ports.MonitorFilter{RestrictToGroupIDs: true, GroupIDs: []int64{parent.ID, child.ID}, GroupIDIsNull: true}, []int64{}},
				{"disabled", ports.MonitorFilter{GroupIDs: []int64{child.ID}}, []int64{root.ID, nested.ID, bare.ID}},
				{"type", ports.MonitorFilter{RestrictToGroupIDs: true, GroupIDs: []int64{parent.ID, child.ID}, Type: "dns"}, []int64{}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					got, err := repos.monitors.List(ctx, tc.filter)
					if err != nil {
						t.Fatal(err)
					}
					ids := make([]int64, 0, len(got))
					for _, m := range got {
						ids = append(ids, m.ID)
					}
					if !reflect.DeepEqual(ids, tc.want) {
						t.Fatalf("got=%v want=%v", ids, tc.want)
					}
				})
			}
			// The service must keep the grant intersection when expanding a
			// selected ancestor, and must refresh it before a cached response.
			access := services.NewAccessService(repos.users, repos.userPermissions, repos.monitorGroups, repos.monitors)
			if err := access.GrantMonitor(ctx, user.ID, nested.ID); err != nil {
				t.Fatal(err)
			}
			svc := services.NewInsightsService(repos.heartbeats.(ports.ReliabilityReader), repos.heartbeats.(ports.AggregateBatchReader), repos.monitors, repos.monitorGroups, access)
			query := services.InsightsQuery{UserID: user.ID, Period: services.Period7d, GroupID: &parent.ID}
			for range 2 {
				got, err := svc.GetInsights(ctx, query)
				if err != nil || len(got.Rows) != 1 || got.Rows[0].MonitorID != nested.ID {
					t.Fatalf("service group selection=%+v err=%v", got, err)
				}
			}
			if err := access.RevokeMonitor(ctx, user.ID, nested.ID); err != nil {
				t.Fatal(err)
			}
			got, err := svc.GetInsights(ctx, query)
			if err != nil || len(got.Rows) != 0 {
				t.Fatalf("revoked cache result=%+v err=%v", got, err)
			}
		})
	}
}
