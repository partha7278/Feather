package notify

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// syncBuffer collects log output from the notifier.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func capturingLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// deadAddress is a host:port nothing listens on, so delivery fails fast with a
// transport error that quotes the URL.
func deadAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// A chat webhook URL is a bearer credential: Slack, Discord and Teams put the
// secret in the path or the query, and anyone holding the URL can post into
// the channel. When delivery fails, Go's transport error quotes the whole URL,
// and that error is what gets logged.
func TestAFailedDeliveryDoesNotLogTheWebhookSecret(t *testing.T) {
	addr := deadAddress(t)
	cases := []struct {
		name, url, secret string
	}{
		{"secret in userinfo", "http://bot:userinfo-secret-1@" + addr + "/hook", "userinfo-secret-1"},
		{"secret in a token query parameter", "http://" + addr + "/hook?token=query-secret-2", "query-secret-2"},
		{"slack-style secret in the path", "http://" + addr + "/services/T0000/B0000/PathSecretXyZ123", "PathSecretXyZ123"},
		{"discord-style secret in the path", "http://" + addr + "/api/webhooks/123/DiscordTokenAbC456", "DiscordTokenAbC456"},
		{"secret in a sig query parameter", "http://" + addr + "/workflows/x?sig=TeamsSigQwE789", "TeamsSigQwE789"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log, buf := capturingLogger()
			New(Settings{WebhookURL: tc.url}, log).Notify(t.Context(), Event{Kind: KindRunFailed})
			out := buf.String()
			if !strings.Contains(out, "could not deliver notification") {
				t.Fatalf("expected a delivery failure to be logged, got: %s", out)
			}
			if strings.Contains(out, tc.secret) {
				t.Errorf("the webhook secret was logged: %s", out)
			}
		})
	}
}

// A rejected notification names the endpoint by host only, so the log helps
// diagnose the problem without carrying the credential in the path.
func TestARejectedDeliveryLogsOnlyTheHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "response-body-secret")
	}))
	defer server.Close()

	log, buf := capturingLogger()
	url := strings.Replace(server.URL, "http://", "http://bot:ui-secret@", 1) + "/services/T1/B2/RejectedPathSecret?token=RejectedQuerySecret"
	New(Settings{WebhookURL: url}, log).Notify(t.Context(), Event{Kind: KindRunFailed})

	out := buf.String()
	if !strings.Contains(out, "notification was rejected") {
		t.Fatalf("expected a rejection to be logged, got: %s", out)
	}
	for _, secret := range []string{"RejectedPathSecret", "RejectedQuerySecret", "ui-secret", "response-body-secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("the rejection log contains %q: %s", secret, out)
		}
	}
}

// Nothing the payload or the configured URL contains can add a request header.
// A URL with a CRLF in it is refused before anything is sent.
func TestAWebhookURLCannotInjectHeaders(t *testing.T) {
	var hits atomic.Int32
	var injected atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("X-Injected") != "" {
			injected.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	for _, url := range []string{
		server.URL + "/hook\r\nX-Injected: 1",
		server.URL + "/hook%0d%0aX-Injected:%201",
		server.URL + "/hook HTTP/1.1\r\nX-Injected: 1\r\n\r\n",
	} {
		New(Settings{WebhookURL: url}, discard()).Notify(t.Context(), Event{
			Kind: KindRunFailed, Summary: "s\r\nX-Injected: 2", ConfigName: "c\r\nX-Injected: 3",
		})
	}
	if injected.Load() {
		t.Fatal("a header was injected into the webhook request")
	}
}

// Only the event goes to the webhook: no cookies, no authorisation the
// operator did not put in the URL, and the content type the receiver expects.
func TestTheWebhookRequestCarriesOnlyWhatItShould(t *testing.T) {
	headers := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	New(Settings{WebhookURL: server.URL}, discard()).Notify(t.Context(), Event{Kind: KindRunFailed})
	got := <-headers
	if got.Get("Authorization") != "" || got.Get("Cookie") != "" {
		t.Errorf("unexpected credentials in the request: %v", got)
	}
	if got.Get("Content-Type") != "application/json" {
		t.Errorf("content type is %q", got.Get("Content-Type"))
	}
}

// A webhook URL with a scheme the HTTP client cannot speak is a failed
// delivery, not a way to read local files.
func TestANonHTTPWebhookIsAFailedDelivery(t *testing.T) {
	for _, url := range []string{"file:///etc/passwd", "gopher://127.0.0.1:70/_x", "ftp://127.0.0.1/x"} {
		log, buf := capturingLogger()
		New(Settings{WebhookURL: url}, log).Notify(t.Context(), Event{Kind: KindRunFailed})
		if strings.Contains(buf.String(), "root:") {
			t.Fatalf("%s returned local file content: %s", url, buf.String())
		}
		if !strings.Contains(buf.String(), "could not") {
			t.Errorf("%s did not fail visibly: %s", url, buf.String())
		}
	}
}

// Every free-text field in the payload is redacted, not just the error. The
// summary is built from the error, so a credential in one is in the other.
func TestEveryFreeTextFieldIsRedacted(t *testing.T) {
	received, server := receiver(t)
	defer server.Close()

	err := "dial mongodb://app:summary-pw@db:27017: SecretAccessKey=AbCdEfGhIjKlMnOpQrStUvWxYz0123456789ABCD"
	New(Settings{WebhookURL: server.URL}, discard()).Notify(t.Context(), Event{
		Kind:    KindRunFailed,
		Summary: Describe("orders", "failed", 0, 0, errString(err)),
		Error:   err,
	})
	event := received.wait(t)
	for _, field := range []string{event.Summary, event.Error} {
		for _, secret := range []string{"summary-pw", "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789ABCD"} {
			if strings.Contains(field, secret) {
				t.Errorf("%q reached the webhook: %s", secret, field)
			}
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
