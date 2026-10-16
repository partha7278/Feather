package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/unifers/feather/internal/config"
	"github.com/unifers/feather/internal/store"
)

const pgPassword = "pg-hunter2"

func seedPostgresAccount(t *testing.T, server *Server) store.PostgresSourceRecord {
	t.Helper()
	record := store.PostgresSourceRecord{
		Alias:   "warehouse",
		DSN:     "postgres://archiver:" + pgPassword + "@127.0.0.1:5432/shop",
		Enabled: true,
	}
	if err := server.app.Store.SavePostgresSource(context.Background(), &record); err != nil {
		t.Fatal(err)
	}
	if err := server.app.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return record
}

func postgresConfigForm() url.Values {
	form := validConfigForm()
	form.Del("mongoSource")
	form.Set("sourceAccount", "postgres:warehouse")
	form.Set("database", "")
	form.Set("collection", "orders")
	form.Set("filter", `created_at < {{now-90d}} AND status = 'closed'`)
	return form
}

func TestSavingAPostgresAccountStoresItAuditsItAndHidesThePassword(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	response := post(t, server, "/postgres", url.Values{
		"alias":   {"warehouse"},
		"dsn":     {"host=db.internal user=archiver password=" + pgPassword + " dbname=shop sslmode=require"},
		"enabled": {"on"},
	}, session)
	assertRedirect(t, response, "/postgres")

	sources, err := server.app.Store.ListPostgresSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Alias != "warehouse" || !sources[0].Enabled {
		t.Fatalf("stored: %+v", sources)
	}
	if strings.Contains(sources[0].Preview, pgPassword) {
		t.Errorf("the stored preview holds the password: %s", sources[0].Preview)
	}
	assertAudited(t, server, store.AuditPostgresSaved, "warehouse")

	for _, path := range []string{"/postgres", "/postgres/" + sources[0].ID} {
		page := get(t, server, path, session)
		if page.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d", path, page.Code)
		}
		if strings.Contains(page.Body.String(), pgPassword) {
			t.Errorf("GET %s renders the password", path)
		}
	}
}

func TestPostgresAccountFormRejectsAMongoURI(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	response := post(t, server, "/postgres", url.Values{
		"alias": {"wrong"}, "dsn": {"mongodb://user:pw@host:27017"},
	}, session)
	assertRedirect(t, response, "/postgres/new")
	if kind := flashKind(response); kind != "error" {
		t.Errorf("flash kind %q, want error", kind)
	}
	if sources, _ := server.app.Store.ListPostgresSources(context.Background()); len(sources) != 0 {
		t.Errorf("an invalid account was stored: %+v", sources)
	}
}

func TestAPostgresConfigIsSavedThroughTheForm(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedAccountsOnly(t, server)
	seedPostgresAccount(t, server)

	response := post(t, server, "/configs", postgresConfigForm(), session)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", response.Code, truncateForLog(response.Body.String()))
	}

	record, err := server.app.Store.JobByName(context.Background(), "orders-archive")
	if err != nil {
		t.Fatal(err)
	}
	src := record.Config.Source
	if !src.IsPostgres() || src.PostgresSource != "warehouse" || src.MongoSource != "" {
		t.Fatalf("stored source: %+v", src)
	}
	if src.Database != "public" || src.Collection != "orders" {
		t.Errorf("stored namespace %s.%s, want public.orders", src.Database, src.Collection)
	}
	if condition, _ := src.SQLFilter(); condition != `created_at < {{now-90d}} AND status = 'closed'` {
		t.Errorf("stored filter %q", condition)
	}

	// The edit form shows the condition as typed, not as a JSON string.
	page := get(t, server, "/configs/"+record.ID, session)
	if page.Code != http.StatusOK {
		t.Fatalf("edit form: status %d", page.Code)
	}
	body := page.Body.String()
	if !strings.Contains(body, `created_at &lt; {{now-90d}} AND status = &#39;closed&#39;`) {
		t.Errorf("the edit form does not show the SQL filter as typed")
	}
	if !strings.Contains(body, `value="postgres:warehouse" selected`) {
		t.Errorf("the edit form does not select the PostgreSQL account")
	}
}

// A filter that would escape its parentheses in the DELETE is refused, and the
// form comes back with what was typed so it can be fixed.
func TestAnUnsafeSQLFilterIsShownBackAndNotSaved(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedAccountsOnly(t, server)
	seedPostgresAccount(t, server)

	form := postgresConfigForm()
	form.Set("filter", `1=1) OR (1=1`)
	response := post(t, server, "/configs", form, session)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d, want the form shown back", response.Code)
	}
	if !strings.Contains(response.Body.String(), `1=1) OR (1=1`) {
		t.Error("the rejected filter was not shown back")
	}
	if jobs, _ := server.app.Store.ListJobs(context.Background()); len(jobs) != 0 {
		t.Fatalf("an unsafe config was saved")
	}
}

func TestAPostgresAccountInUseCannotBeRemoved(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)
	seedAccountsOnly(t, server)
	account := seedPostgresAccount(t, server)

	if response := post(t, server, "/configs", postgresConfigForm(), session); response.Code != http.StatusSeeOther {
		t.Fatalf("saving the config: status %d", response.Code)
	}

	response := post(t, server, "/postgres/"+account.ID+"/delete", url.Values{}, session)
	assertRedirect(t, response, "/postgres")
	if kind := flashKind(response); kind != "error" {
		t.Errorf("flash kind %q, want error", kind)
	}
	err := server.app.Store.DeletePostgresSource(context.Background(), account.ID)
	var inUse *store.ErrInUse
	if !errors.As(err, &inUse) {
		t.Fatalf("DeletePostgresSource = %v, want ErrInUse", err)
	}

	// The MongoDB account the seed made is not held by a PostgreSQL config.
	mongo, _ := server.app.Store.ListMongoSources(context.Background())
	if err := server.app.Store.DeleteMongoSource(context.Background(), mongo[0].ID); err != nil {
		t.Errorf("deleting an unused MongoDB account: %v", err)
	}
}

func TestSourceAccountParsing(t *testing.T) {
	cases := []struct {
		values       url.Values
		engine, name string
	}{
		{url.Values{"sourceAccount": {"postgres:warehouse"}}, config.EnginePostgres, "warehouse"},
		{url.Values{"sourceAccount": {"mongodb:prod"}}, config.EngineMongo, "prod"},
		{url.Values{"mongoSource": {"prod"}}, config.EngineMongo, "prod"},
	}
	for _, tc := range cases {
		engine, alias := sourceAccountFromForm(postedForm(tc.values))
		if engine != tc.engine || alias != tc.name {
			t.Errorf("%v: got %s:%s, want %s:%s", tc.values, engine, alias, tc.engine, tc.name)
		}
	}
}
