package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/unifers/feather/internal/store"
)

// validConfigForm is a submission the config form accepts against the accounts
// seedAccountsOnly creates. Tests copy it and break one field.
func validConfigForm() url.Values {
	return url.Values{
		"name":           {"orders-archive"},
		"enabled":        {"on"},
		"mongoSource":    {"prod"},
		"database":       {"shop"},
		"collection":     {"orders"},
		"filter":         {`{"status":"completed"}`},
		"destinations":   {"cold"},
		"keyTemplate":    {"archive/{collection}/{runId}-{seq}{ext}"},
		"codec":          {"gzip"},
		"codecLevel":     {"6"},
		"batchMaxBytes":  {"8MB"},
		"scheduleKind":   {"every"},
		"every":          {"1d"},
		"at":             {"02:30"},
		"timezone":       {"UTC"},
		"maxRuntime":     {"6h"},
		"readPreference": {"primary"},
	}
}

func TestSavingAConfigIsAudited(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedAccountsOnly(t, server)

	response := post(t, server, "/configs", validConfigForm(), session)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", response.Code, truncateForLog(response.Body.String()))
	}
	if events := auditEvents(t, server, store.AuditConfigSaved); len(events) != 1 {
		t.Fatalf("recorded %d config-saved events, want 1", len(events))
	}
}

// The re-render path is the user's uncommitted change: a rejected submission
// is shown back (200) instead of redirected, and nothing is written. Each case
// fails somewhere different — in form parsing, or later in the store.
func TestARejectedConfigIsShownBackAndNotSaved(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(url.Values)
	}{
		{"a filter that is not JSON", func(f url.Values) { f.Set("filter", `{"a":`) }},
		{"a projection that is not JSON", func(f url.Values) { f.Set("projection", `nope`) }},
		{"a batch size that is not a size", func(f url.Values) { f.Set("batchMaxBytes", "lots") }},
		{"a runtime that is not a duration", func(f url.Values) { f.Set("maxRuntime", "forever") }},
		{"a MongoDB account that does not exist", func(f url.Values) { f.Set("mongoSource", "nope") }},
		{"a destination that does not exist", func(f url.Values) { f.Set("destinations", "nowhere") }},
		{"no name", func(f url.Values) { f.Del("name") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := testServer(t)
			createAdmin(t, server)
			session := signIn(t, server)
			seedAccountsOnly(t, server)

			form := validConfigForm()
			tc.mutate(form)
			response := post(t, server, "/configs", form, session)
			if response.Code != http.StatusOK {
				t.Fatalf("status %d, want the form shown back with 200", response.Code)
			}
			if ct := response.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
				t.Errorf("content type %q", ct)
			}
			jobs, err := server.app.Store.ListJobs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(jobs) != 0 {
				t.Errorf("a rejected config was saved: %d configs", len(jobs))
			}
			assertNotAudited(t, server, store.AuditConfigSaved)
		})
	}
}

// Rejecting an edit must leave the stored config exactly as it was.
func TestARejectedEditLeavesTheStoredConfigAlone(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	_, _, jobID := seedData(t, server)

	form := validConfigForm()
	form.Set("collection", "invoices")
	form.Set("filter", `{"broken":`)
	response := post(t, server, "/configs/"+jobID, form, session)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d, want the form shown back", response.Code)
	}

	stored, err := server.app.Store.JobByID(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Config.Source.Collection != "orders" {
		t.Errorf("the rejected edit changed the stored collection to %q", stored.Config.Source.Collection)
	}
}

// An edit to a config that has since been removed is a 404, not a 500 and not
// a silent re-create.
func TestARejectedEditOfAMissingConfigIsNotFound(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedAccountsOnly(t, server)

	form := validConfigForm()
	form.Set("filter", `{"broken":`)
	response := post(t, server, "/configs/00000000-0000-0000-0000-000000000000", form, session)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", response.Code)
	}
}

func TestEditingAConfigUpdatesItInPlace(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	_, _, jobID := seedData(t, server)

	form := validConfigForm()
	form.Set("collection", "invoices")
	form.Set("scheduleKind", "cron")
	form.Set("cron", "15 3 * * *")
	response := post(t, server, "/configs/"+jobID, form, session)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", response.Code, truncateForLog(response.Body.String()))
	}

	jobs, err := server.app.Store.ListJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("editing produced %d configs, want 1", len(jobs))
	}
	if jobs[0].ID != jobID || jobs[0].Config.Source.Collection != "invoices" {
		t.Errorf("stored %s on %q", jobs[0].ID, jobs[0].Config.Source.Collection)
	}
	if jobs[0].Config.Schedule.Cron != "15 3 * * *" {
		t.Errorf("cron schedule %q was not saved", jobs[0].Config.Schedule.Cron)
	}
}

func TestPausingAndResumingAConfig(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	_, _, jobID := seedData(t, server)
	ctx := context.Background()

	steps := []struct {
		value   string
		enabled bool
		action  string
	}{
		{"", false, store.AuditConfigDisabled},
		{"on", true, store.AuditConfigEnabled},
		{"false", false, store.AuditConfigDisabled},
		{"true", true, store.AuditConfigEnabled},
	}
	for _, step := range steps {
		before := len(auditEvents(t, server, step.action))
		response := post(t, server, "/configs/"+jobID+"/enabled", url.Values{"enabled": {step.value}}, session)
		assertRedirect(t, response, "/configs")

		record, err := server.app.Store.JobByID(ctx, jobID)
		if err != nil {
			t.Fatal(err)
		}
		if record.Config.IsEnabled() != step.enabled {
			t.Errorf("enabled=%q: config enabled is %v", step.value, record.Config.IsEnabled())
		}
		if after := len(auditEvents(t, server, step.action)); after != before+1 {
			t.Errorf("enabled=%q: %q not audited", step.value, step.action)
		}
	}
}

// Removing a config keeps the catalogue: the archives are still in the bucket.
func TestDeletingAConfigKeepsItsRunsAndArchives(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	_, _, jobID := seedData(t, server)
	ctx := context.Background()

	response := post(t, server, "/configs/"+jobID+"/delete", url.Values{}, session)
	assertRedirect(t, response, "/configs")

	if _, err := server.app.Store.JobByID(ctx, jobID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the config is still there: %v", err)
	}
	runs, err := server.app.Store.RecentRuns(ctx, "orders-archive", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Errorf("run history has %d runs after removing the config, want 1", len(runs))
	}
	artifacts, _, err := server.app.Store.Artifacts(ctx, store.ArtifactQuery{ConfigName: "orders-archive"})
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 {
		t.Errorf("catalogue has %d archives after removing the config, want 1", len(artifacts))
	}
	assertAudited(t, server, store.AuditConfigDeleted, "orders-archive")
}

func TestCollectionPickerAPI(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	t.Run("a missing source alias is a bad request", func(t *testing.T) {
		response := get(t, server, "/api/collections", session)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", response.Code)
		}
	})

	t.Run("an unknown source alias is reported in JSON", func(t *testing.T) {
		response := get(t, server, "/api/collections?source=nope", session)
		if response.Code != http.StatusOK {
			t.Fatalf("status %d, want 200", response.Code)
		}
		if ct := response.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
			t.Errorf("content type %q", ct)
		}
		var body map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("not JSON: %v", err)
		}
		if body["error"] == nil || body["error"] == "" {
			t.Errorf("no error reported: %v", body)
		}
	})

	t.Run("it needs a session", func(t *testing.T) {
		response := get(t, server, "/api/collections?source=prod", "")
		if response.Code != http.StatusSeeOther {
			t.Fatalf("status %d, want a redirect to sign in", response.Code)
		}
	})
}

func TestScheduleKindFollowsTheConfig(t *testing.T) {
	job := newJobTemplate()
	if got := scheduleKind(job); got != "every" {
		t.Errorf("a blank schedule is %q, want every", got)
	}
	job.Schedule.Cron = "0 3 * * *"
	if got := scheduleKind(job); got != "cron" {
		t.Errorf("a cron schedule is %q, want cron", got)
	}
}

func TestNewConfigsStartWithDeletionOff(t *testing.T) {
	if newJobTemplate().Retention.DeleteAfterArchive {
		t.Fatal("a blank config starts with deletion on")
	}
}

func TestConfigFormPath(t *testing.T) {
	if got := configFormPath(""); got != "/configs/new" {
		t.Errorf("new: %q", got)
	}
	if got := configFormPath("abc"); got != "/configs/abc" {
		t.Errorf("edit: %q", got)
	}
}
