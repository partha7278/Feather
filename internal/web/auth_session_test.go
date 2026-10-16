package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The login throttle is process-wide, and every httptest request comes from
// the same address. Clear what a test left behind so a later test's sign-in
// is not refused for an earlier test's failures.
func resetLoginThrottle(t *testing.T, emails ...string) {
	t.Cleanup(func() {
		for _, email := range emails {
			ipKey, accountKey := throttleKeys("192.0.2.1", email)
			loginThrottle.succeed(ipKey)
			loginThrottle.succeed(accountKey)
		}
	})
}

func TestSigningInStartsASessionAndGoesToNext(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	resetLoginThrottle(t, "ops@example.com")

	cases := []struct {
		name string
		next string
		want string
	}{
		{"no next goes to the dashboard", "", "/"},
		{"a local next is followed", "/configs?page=2", "/configs?page=2"},
		{"an off-site next is ignored", "https://evil.example/", "/"},
		{"a protocol-relative next is ignored", "//evil.example/", "/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := postWithPreSessionCSRF(t, server, "/login", url.Values{
				"email": {"ops@example.com"}, "password": {"correct-horse-battery"}, "next": {tc.next},
			})
			assertRedirect(t, response, tc.want)

			token := sessionValue(response)
			if token == "" {
				t.Fatal("no session cookie was set")
			}
			if _, user, err := server.app.Store.SessionByToken(context.Background(), token); err != nil || user.Email != "ops@example.com" {
				t.Fatalf("the cookie does not name a live session: %v", err)
			}
			for _, cookie := range response.Result().Cookies() {
				if cookie.Name == sessionCookie {
					if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
						t.Errorf("session cookie is not HttpOnly/Lax: %+v", cookie)
					}
				}
			}
			// The cookie is accepted by a guarded page.
			if code := get(t, server, "/configs", token).Code; code != http.StatusOK {
				t.Errorf("a guarded page answered %d with the new session", code)
			}
		})
	}
}

func TestAWrongPasswordDoesNotStartASession(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	resetLoginThrottle(t, "ops@example.com", "nobody@example.com")

	for _, email := range []string{"ops@example.com", "nobody@example.com"} {
		response := postWithPreSessionCSRF(t, server, "/login", url.Values{
			"email": {email}, "password": {"wrong-password-entirely"},
		})
		if response.Code != http.StatusOK {
			t.Errorf("%s: status %d, want the form shown again", email, response.Code)
		}
		if sessionValue(response) != "" {
			t.Errorf("%s: a session cookie was set for a failed sign-in", email)
		}
	}
}

func TestSigningInWithoutTheFormTokenIsRefused(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)

	form := url.Values{"email": {"ops@example.com"}, "password": {"correct-horse-battery"}}
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	assertRedirect(t, response, "/login")
	if sessionValue(response) != "" {
		t.Fatal("a login post without its CSRF token started a session")
	}
}

// After enough failures the right password is refused too, until the
// throttle lifts — otherwise the throttle only slows guesses that were wrong.
func TestTheThrottleRefusesEvenTheRightPassword(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	resetLoginThrottle(t, "ops@example.com")

	for range throttleAfter {
		postWithPreSessionCSRF(t, server, "/login", url.Values{
			"email": {"ops@example.com"}, "password": {"wrong-password-entirely"},
		})
	}
	response := postWithPreSessionCSRF(t, server, "/login", url.Values{
		"email": {"ops@example.com"}, "password": {"correct-horse-battery"},
	})
	if sessionValue(response) != "" {
		t.Fatal("a throttled account was signed in")
	}
}

func TestSigningOutEndsTheSession(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)
	session := signIn(t, server)

	request := httptest.NewRequest(http.MethodPost, "/logout", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	assertRedirect(t, response, "/login")
	if _, _, err := server.app.Store.SessionByToken(context.Background(), session); err == nil {
		t.Error("the session survived signing out")
	}
	cleared := false
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == sessionCookie && cookie.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the session cookie was not cleared")
	}
	if code := get(t, server, "/configs", session).Code; code != http.StatusSeeOther {
		t.Errorf("the old cookie still opens a guarded page: %d", code)
	}
}

func TestSetupIsGoneOnceAnAdministratorExists(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)

	// Once an administrator exists, /setup no longer exists.
	assertRedirect(t, get(t, server, "/setup", ""), "/login")
}

func TestASignedOutVisitRemembersWhereItWasGoing(t *testing.T) {
	server := testServer(t)
	createAdmin(t, server)

	response := get(t, server, "/runs?config=orders", "")
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want a redirect to sign in", response.Code)
	}
	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Path != "/login" {
		t.Fatalf("redirected to %q", location)
	}
	if next := location.Query().Get("next"); next != "/runs?config=orders" {
		t.Errorf("next = %q, want the original page", next)
	}
}
