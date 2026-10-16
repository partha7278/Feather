package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func postedForm(values url.Values) *form {
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	f, err := newForm(request)
	if err != nil {
		panic(err)
	}
	return f
}

func TestFormReadsAndCollectsEveryProblem(t *testing.T) {
	f := postedForm(url.Values{
		"name":    {"  orders  "},
		"filter":  {"  {\n  \"a\": 1\n}  "},
		"enabled": {"Yes"},
		"off":     {"nope"},
		"size":    {"12"},
		"bad":     {"twelve"},
		"tags":    {" a ", "", "b"},
	})

	if got := f.String("name"); got != "orders" {
		t.Errorf("String trims: %q", got)
	}
	if got := f.Raw("filter"); !strings.HasPrefix(got, "  {\n") {
		t.Errorf("Raw keeps whitespace: %q", got)
	}
	if !f.Bool("enabled") || f.Bool("off") || f.Bool("missing") {
		t.Error("Bool misread a checkbox")
	}
	if got := f.Int("size", 0); got != 12 {
		t.Errorf("Int = %d", got)
	}
	if got := f.Int("missing", 7); got != 7 {
		t.Errorf("Int fallback = %d", got)
	}
	if got := f.Strings("tags"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("Strings = %q", got)
	}
	if !f.OK() {
		t.Fatalf("no problems yet, but: %s", f.Error())
	}

	if got := f.Int("bad", 3); got != 3 {
		t.Errorf("a bad Int should fall back, got %d", got)
	}
	f.Require("deleteBatchSize")
	if f.OK() {
		t.Fatal("two problems were not recorded")
	}
	if len(f.Errors) != 2 {
		t.Errorf("recorded %d problems, want 2: %q", len(f.Errors), f.Errors)
	}
	// Both are reported at once, naming the fields readably.
	if !strings.Contains(f.Error(), "Bad") || !strings.Contains(f.Error(), "Delete batch size") {
		t.Errorf("error does not name both fields: %q", f.Error())
	}
}

func TestFieldLabels(t *testing.T) {
	cases := map[string]string{
		"deleteBatchSize": "Delete batch size",
		"alias":           "Alias",
		"x":               "X",
		"":                "",
	}
	for in, want := range cases {
		if got := label(in); got != want {
			t.Errorf("label(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFlashSurvivesOneRoundTrip(t *testing.T) {
	server := testServer(t)

	set := httptest.NewRecorder()
	server.setFlash(set, "error", "%s could not be reached: %s", "prod|eu", "timeout; 100% sure")

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, cookie := range set.Result().Cookies() {
		request.AddCookie(cookie)
	}
	taken := httptest.NewRecorder()
	got := takeFlash(taken, request)
	if got == nil {
		t.Fatal("the flash was lost")
	}
	if got.Kind != "error" || got.Message != "prod|eu could not be reached: timeout; 100% sure" {
		t.Errorf("flash = %+v", got)
	}

	// Taking it clears it, so it shows once.
	cleared := false
	for _, cookie := range taken.Result().Cookies() {
		if cookie.Name == flashCookie && cookie.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("taking the flash did not clear it")
	}
}

func TestMalformedFlashCookiesAreIgnored(t *testing.T) {
	for _, value := range []string{"", "no-separator", "%zz"} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.AddCookie(&http.Cookie{Name: flashCookie, Value: value})
		if got := takeFlash(httptest.NewRecorder(), request); got != nil {
			t.Errorf("cookie %q produced a flash: %+v", value, got)
		}
	}
}

func TestHumanNumber(t *testing.T) {
	cases := map[int64]string{
		0: "0", 999: "999", 1000: "1,000", 12345: "12,345", 1234567: "1,234,567", 100000000: "100,000,000",
	}
	for in, want := range cases {
		if got := humanNumber(in); got != want {
			t.Errorf("humanNumber(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{500 * time.Millisecond, "less than a second"},
		{42 * time.Second, "42s"},
		{3*time.Minute + 7*time.Second, "3m 7s"},
		{5*time.Hour + 20*time.Minute, "5h 20m"},
		{50 * time.Hour, "2d 2h"},
	}
	for _, tc := range cases {
		if got := humanDuration(tc.in); got != tc.want {
			t.Errorf("humanDuration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHumanSince(t *testing.T) {
	var nilTime *time.Time
	if got := humanSince(nilTime); got != "never" {
		t.Errorf("nil: %q", got)
	}
	if got := humanSince(time.Time{}); got != "never" {
		t.Errorf("zero: %q", got)
	}
	if got := humanSince("not a time"); got != "never" {
		t.Errorf("wrong type: %q", got)
	}
	if got := humanSince(time.Now().Add(-90 * time.Second)); !strings.HasSuffix(got, " ago") {
		t.Errorf("past: %q", got)
	}
	future := time.Now().Add(2 * time.Hour)
	if got := humanSince(&future); !strings.HasPrefix(got, "in ") {
		t.Errorf("future: %q", got)
	}
}

func TestDateFormattingHandlesMissingValues(t *testing.T) {
	var nilTime *time.Time
	for _, value := range []any{nilTime, time.Time{}, "nope", nil} {
		if got := formatDateTime(value); got != "—" {
			t.Errorf("formatDateTime(%#v) = %q", value, got)
		}
		if got := formatDate(value); got != "—" {
			t.Errorf("formatDate(%#v) = %q", value, got)
		}
	}
	at := time.Date(2026, 3, 4, 5, 6, 0, 0, time.Local)
	if got := formatDate(&at); got != "4 Mar 2026" {
		t.Errorf("formatDate = %q", got)
	}
	if got := formatDateTime(at); got != "4 Mar 2026, 05:06" {
		t.Errorf("formatDateTime = %q", got)
	}
}

func TestStatusTone(t *testing.T) {
	cases := map[string]string{
		"success": "good", "verified": "good", "ok": "good",
		"running": "busy", "uploaded": "busy", "pending": "busy",
		"partial": "warn", "dry_run": "warn", "skipped": "warn",
		"failed":  "bad",
		"unknown": "muted", "": "muted",
	}
	for status, want := range cases {
		if got := statusTone(status); got != want {
			t.Errorf("statusTone(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestJSONHelpersFallBackToTheInput(t *testing.T) {
	if got := prettyJSON(`{"a":1}`); got != "{\n  \"a\": 1\n}" {
		t.Errorf("prettyJSON = %q", got)
	}
	if got := compactJSON("{\n  \"a\": 1\n}"); got != `{"a":1}` {
		t.Errorf("compactJSON = %q", got)
	}
	for _, broken := range []string{`{"a":`, "not json"} {
		if got := prettyJSON(broken); got != broken {
			t.Errorf("prettyJSON(%q) = %q", broken, got)
		}
		if got := compactJSON(broken); got != broken {
			t.Errorf("compactJSON(%q) = %q", broken, got)
		}
	}
}

func TestQueryStringDropsEmptyValues(t *testing.T) {
	if got := queryString("config", "orders", "page", 2, "q", "", "deleted", 0); got != "?config=orders&page=2" {
		t.Errorf("queryString = %q", got)
	}
	if got := queryString("q", ""); got != "" {
		t.Errorf("all-empty queryString = %q", got)
	}
	if got := queryString("odd"); got != "" {
		t.Errorf("unpaired queryString = %q", got)
	}
}

func TestSmallTemplateHelpers(t *testing.T) {
	if got := dict("a", 1, "b", "two", "dangling"); !reflect.DeepEqual(got, map[string]any{"a": 1, "b": "two"}) {
		t.Errorf("dict = %v", got)
	}
	if percent(1, 0) != 0 || percent(1, 4) != 25 || percent(3, 3) != 100 {
		t.Error("percent is wrong")
	}
	if truncateText(3, "abcdef") != "abc…" || truncateText(10, "abc") != "abc" {
		t.Error("truncateText is wrong")
	}
	if !sliceContains([]string{"a", "b"}, "b") || sliceContains(nil, "a") {
		t.Error("sliceContains is wrong")
	}
	if auditLabel("config.saved") == "" || auditLabel("something.new") != "something.new" {
		t.Error("auditLabel should name known actions and fall back to the raw one")
	}
}

func TestEveryAuditActionHasALabel(t *testing.T) {
	seen := map[string]bool{}
	for _, entry := range auditActionLabels {
		if entry.Label == "" || entry.Label == entry.Action {
			t.Errorf("%q has no readable label", entry.Action)
		}
		if seen[entry.Action] {
			t.Errorf("%q is listed twice", entry.Action)
		}
		seen[entry.Action] = true
	}
}
