package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestThemeCookieSetsDataThemeOnThePage(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	cases := []struct {
		cookie string
		want   string
	}{
		{"light", `<html lang="en" data-theme="light">`},
		{"dark", `<html lang="en" data-theme="dark">`},
		{"", `<html lang="en">`},
		{"neon", `<html lang="en">`}, // anything else follows the system
	}
	for _, tc := range cases {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		if tc.cookie != "" {
			request.AddCookie(&http.Cookie{Name: themeCookie, Value: tc.cookie})
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)

		if !strings.Contains(response.Body.String(), tc.want) {
			t.Errorf("cookie %q: page does not start with %s", tc.cookie, tc.want)
		}
	}
}

func TestThemeSwitchFallbackSetsTheCookieAndGoesBack(t *testing.T) {
	server := testServer(t)
	cases := []struct {
		theme  string
		value  string
		maxAge int
	}{
		{"dark", "dark", 400 * 24 * 60 * 60},
		{"light", "light", 400 * 24 * 60 * 60},
		{"system", "", -1},
	}
	for _, tc := range cases {
		request := httptest.NewRequest(http.MethodPost, "http://example.com/theme",
			strings.NewReader("theme="+tc.theme))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Referer", "http://example.com/configs")
		response := httptest.NewRecorder()
		server.handleTheme(response, request)

		assertRedirect(t, response, "/configs")
		var found bool
		for _, cookie := range response.Result().Cookies() {
			if cookie.Name != themeCookie {
				continue
			}
			found = true
			if cookie.Value != tc.value || cookie.MaxAge != tc.maxAge {
				t.Errorf("%s: cookie %q max-age %d, want %q max-age %d", tc.theme, cookie.Value, cookie.MaxAge, tc.value, tc.maxAge)
			}
		}
		if !found {
			t.Errorf("%s: no theme cookie set", tc.theme)
		}
	}
}
