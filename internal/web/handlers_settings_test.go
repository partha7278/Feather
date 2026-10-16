package web

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unifers/feather/internal/app"
	"github.com/unifers/feather/internal/store"
)

func TestSavingSettingsStoresThemAndAuditsIt(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	ctx := context.Background()

	response := post(t, server, "/settings", url.Values{
		"instanceName":    {"prod-eu"},
		"maxConcurrent":   {"4"},
		"webhookURL":      {"  https://hooks.example.com/feather  "},
		"notifyOnSuccess": {"on"},
	}, session)
	assertRedirect(t, response, "/settings")
	if kind := flashKind(response); kind != "ok" {
		t.Errorf("flash kind %q, want ok", kind)
	}

	want := map[string]string{
		app.MetaInstanceName:      "prod-eu",
		app.MetaMaxConcurrentJobs: "4",
		app.MetaWebhookURL:        "https://hooks.example.com/feather",
		app.MetaNotifyOnSuccess:   "1",
	}
	for key, value := range want {
		got, err := server.app.Store.Meta(ctx, key, "")
		if err != nil {
			t.Fatal(err)
		}
		if got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
	if events := auditEvents(t, server, store.AuditSettingsSaved); len(events) != 1 {
		t.Errorf("recorded %d settings-saved events, want 1", len(events))
	}
}

func TestSettingsRejectsOutOfRangeValues(t *testing.T) {
	cases := []struct {
		name string
		form url.Values
	}{
		{"zero concurrent jobs", url.Values{"maxConcurrent": {"0"}}},
		{"too many concurrent jobs", url.Values{"maxConcurrent": {"17"}}},
		{"a webhook without a scheme", url.Values{"maxConcurrent": {"2"}, "webhookURL": {"hooks.example.com/x"}}},
		{"a webhook on another scheme", url.Values{"maxConcurrent": {"2"}, "webhookURL": {"ftp://hooks.example.com/x"}}},
		{"a webhook with no host", url.Values{"maxConcurrent": {"2"}, "webhookURL": {"https:///x"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := testServer(t)
			createAdmin(t, server)
			session := signIn(t, server)

			response := post(t, server, "/settings", tc.form, session)
			assertRedirect(t, response, "/settings")
			if kind := flashKind(response); kind != "error" {
				t.Errorf("flash kind %q, want error", kind)
			}
			webhook, err := server.app.Store.Meta(context.Background(), app.MetaWebhookURL, "unset")
			if err != nil {
				t.Fatal(err)
			}
			if webhook != "unset" {
				t.Errorf("a rejected submission stored webhook %q", webhook)
			}
			assertNotAudited(t, server, store.AuditSettingsSaved)
		})
	}
}

func TestAddingAnAdministrator(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	response := post(t, server, "/settings/admins", url.Values{
		"email": {"Second@Example.com"}, "password": {"another-long-passphrase"},
	}, session)
	assertRedirect(t, response, "/settings")

	admins, err := server.app.Store.ListAdmins(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(admins) != 2 {
		t.Fatalf("%d administrators, want 2", len(admins))
	}
	assertAudited(t, server, store.AuditAdminCreated, "second@example.com")
}

func TestAddingAnAdministratorRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		form url.Values
	}{
		{"no email", url.Values{"password": {"another-long-passphrase"}}},
		{"not an email", url.Values{"email": {"nobody"}, "password": {"another-long-passphrase"}}},
		{"a weak password", url.Values{"email": {"x@example.com"}, "password": {"short"}}},
		{"an email already in use", url.Values{"email": {"ops@example.com"}, "password": {"another-long-passphrase"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := testServer(t)
			createAdmin(t, server)
			session := signIn(t, server)

			response := post(t, server, "/settings/admins", tc.form, session)
			assertRedirect(t, response, "/settings")
			if kind := flashKind(response); kind != "error" {
				t.Errorf("flash kind %q, want error", kind)
			}
			admins, err := server.app.Store.ListAdmins(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(admins) != 1 {
				t.Errorf("%d administrators after a rejected add, want 1", len(admins))
			}
		})
	}
}

func TestRemovingAdministrators(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	ctx := context.Background()
	me := adminID(t, server)

	t.Run("not the one you are signed in as", func(t *testing.T) {
		response := post(t, server, "/settings/admins/"+me+"/delete", url.Values{}, session)
		assertRedirect(t, response, "/settings")
		if kind := flashKind(response); kind != "error" {
			t.Errorf("flash kind %q, want error", kind)
		}
		if admins, _ := server.app.Store.ListAdmins(ctx); len(admins) != 1 {
			t.Fatalf("%d administrators, want 1", len(admins))
		}
	})

	t.Run("another one, and their sessions go with them", func(t *testing.T) {
		other, err := server.app.Store.CreateAdmin(ctx, "other@example.com", "another-long-passphrase")
		if err != nil {
			t.Fatal(err)
		}
		otherSession, _, err := server.app.Store.CreateSession(ctx, other.ID, "test", "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}

		response := post(t, server, "/settings/admins/"+other.ID+"/delete", url.Values{}, session)
		assertRedirect(t, response, "/settings")
		if admins, _ := server.app.Store.ListAdmins(ctx); len(admins) != 1 {
			t.Fatalf("%d administrators, want 1", len(admins))
		}
		if _, _, err := server.app.Store.SessionByToken(ctx, otherSession); err == nil {
			t.Error("the removed administrator is still signed in")
		}
		assertAudited(t, server, store.AuditAdminDeleted, other.ID)
	})
}

func TestPasswordChangeRejectsMismatchedPasswords(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	response := post(t, server, "/settings/password", url.Values{
		"currentPassword": {"correct-horse-battery"},
		"newPassword":     {"a-brand-new-passphrase"},
		"confirmPassword": {"a-different-passphrase"},
	}, session)
	assertRedirect(t, response, "/settings")
	if kind := flashKind(response); kind != "error" {
		t.Errorf("flash kind %q, want error", kind)
	}
	if _, err := server.app.Store.Authenticate(context.Background(), "ops@example.com", "correct-horse-battery"); err != nil {
		t.Errorf("the old password stopped working: %v", err)
	}
	if _, _, err := server.app.Store.SessionByToken(context.Background(), session); err != nil {
		t.Error("a rejected change signed the session out")
	}
}

func TestPasswordChangeRejectsAWrongCurrentPassword(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	response := post(t, server, "/settings/password", url.Values{
		"currentPassword": {"not-my-password"},
		"newPassword":     {"a-brand-new-passphrase"},
		"confirmPassword": {"a-brand-new-passphrase"},
	}, session)
	assertRedirect(t, response, "/settings")
	if kind := flashKind(response); kind != "error" {
		t.Errorf("flash kind %q, want error", kind)
	}
	if _, err := server.app.Store.Authenticate(context.Background(), "ops@example.com", "a-brand-new-passphrase"); err == nil {
		t.Error("the password was changed without the current one")
	}
	assertNotAudited(t, server, store.AuditPasswordChanged)
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{5 << 20, "5.0 MiB"},
		{3 << 30, "3.0 GiB"},
	}
	for _, tc := range cases {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBoolMeta(t *testing.T) {
	if boolMeta(true) != "1" || boolMeta(false) != "0" {
		t.Error("checkbox values are not stored as 1/0")
	}
}

func TestSavingDatabaseBackupSettingsTakesTodaysBackup(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	dir := filepath.Join(t.TempDir(), "backups")

	response := post(t, server, "/settings/db-backups", url.Values{
		"selfBackupKeep": {"10"},
		"selfBackupDir":  {dir},
	}, session)
	assertRedirect(t, response, "/settings")
	if kind := flashKind(response); kind != "ok" {
		t.Errorf("flash kind %q, want ok", kind)
	}
	backups, err := app.ListSelfBackups(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("%d backups after saving, want 1", len(backups))
	}

	page := get(t, server, "/settings", session)
	if !strings.Contains(page.Body.String(), backups[0].Path) {
		t.Error("the settings page does not list the backup")
	}
}

func TestDatabaseBackupSettingsRejectBadValues(t *testing.T) {
	cases := map[string]url.Values{
		"a negative count": {"selfBackupKeep": {"-1"}},
		"too many":         {"selfBackupKeep": {"366"}},
		"a relative dir":   {"selfBackupKeep": {"5"}, "selfBackupDir": {"backups"}},
	}
	for name, form := range cases {
		t.Run(name, func(t *testing.T) {
			server := testServer(t)
			createAdmin(t, server)
			session := signIn(t, server)

			response := post(t, server, "/settings/db-backups", form, session)
			if kind := flashKind(response); kind != "error" {
				t.Errorf("flash kind %q, want error", kind)
			}
			keep, _ := server.app.Store.MetaInt(context.Background(), app.MetaSelfBackupKeep, 0)
			if keep != 0 {
				t.Errorf("stored keep=%d after a rejected save", keep)
			}
		})
	}
}
