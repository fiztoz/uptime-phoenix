package repository_test

import (
	"context"
	"os"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// TestCreatePersistsExplicitFalse pins #117 and #135. Bun replaces a zero
// value with DEFAULT when the field carries a SQL default tag, so a status page
// created unpublished, or a notification created inactive, was stored with the
// column default true. Both engines must store the caller's false.
func TestCreatePersistsExplicitFalse(t *testing.T) {
	engines := map[string]repositoryFactory{"sqlite": sqliteFactory, "mariadb": mariadbFactory}
	for _, name := range []string{"sqlite", "mariadb"} {
		factory := engines[name]
		t.Run(name, func(t *testing.T) {
			if name == "mariadb" && os.Getenv("TEST_MARIADB_DSN") == "" {
				t.Skip("TEST_MARIADB_DSN is unset")
			}
			t.Run("StatusPageUnpublished_117", func(t *testing.T) {
				repos := factory(t)
				ctx := context.Background()
				sp := &domain.StatusPage{Slug: "private-117", Title: "Private", Theme: "auto", Published: false}
				if err := repos.statusPages.Create(ctx, sp); err != nil {
					t.Fatal(err)
				}
				got, err := repos.statusPages.GetBySlug(ctx, sp.Slug)
				if err != nil {
					t.Fatal(err)
				}
				if got.Published {
					t.Fatalf("status page created with published=false was stored published=true")
				}
			})
			t.Run("NotificationInactive_135", func(t *testing.T) {
				repos := factory(t)
				ctx := context.Background()
				user := createUser(t, ctx, repos, "inactive-notif-user")
				n := &domain.Notification{UserID: user.ID, Name: "off", Type: "webhook", Active: false,
					Config: map[string]any{"url": "https://example.test/off"}}
				if err := repos.notifications.Create(ctx, n); err != nil {
					t.Fatal(err)
				}
				got, err := repos.notifications.GetByID(ctx, n.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.Active {
					t.Fatalf("notification created with active=false was stored active=true")
				}
			})
			t.Run("TrueStillTrue", func(t *testing.T) {
				repos := factory(t)
				ctx := context.Background()
				user := createUser(t, ctx, repos, "active-notif-user")
				sp := &domain.StatusPage{Slug: "public-117", Title: "Public", Theme: "auto", Published: true}
				n := &domain.Notification{UserID: user.ID, Name: "on", Type: "webhook", Active: true,
					Config: map[string]any{"url": "https://example.test/on"}}
				if err := repos.statusPages.Create(ctx, sp); err != nil {
					t.Fatal(err)
				}
				if err := repos.notifications.Create(ctx, n); err != nil {
					t.Fatal(err)
				}
				gotSP, err := repos.statusPages.GetBySlug(ctx, sp.Slug)
				if err != nil {
					t.Fatal(err)
				}
				gotN, err := repos.notifications.GetByID(ctx, n.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !gotSP.Published || !gotN.Active {
					t.Fatalf("explicit true lost: published=%v active=%v", gotSP.Published, gotN.Active)
				}
			})
		})
	}
}
