package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAFailedRunIsDelivered(t *testing.T) {
	received, server := receiver(t)
	defer server.Close()

	New(Settings{WebhookURL: server.URL}, discard()).Notify(t.Context(), Event{
		Kind: KindRunFailed, ConfigName: "orders-archive", Status: "failed",
		Summary: "feather: it went wrong",
	})

	event := received.wait(t)
	if event.Kind != KindRunFailed {
		t.Errorf("kind is %q, want %q", event.Kind, KindRunFailed)
	}
	if event.ConfigName != "orders-archive" {
		t.Errorf("config is %q", event.ConfigName)
	}
}

// Credentials reach this package inside driver errors. This is the last place
// the text passes through before it leaves the machine, so it is the last
// chance to take them out.
func TestCredentialsAreRedactedBeforeLeavingTheMachine(t *testing.T) {
	received, server := receiver(t)
	defer server.Close()

	New(Settings{WebhookURL: server.URL}, discard()).Notify(t.Context(), Event{
		Kind:    KindRunFailed,
		Summary: `connecting to mongodb://admin:hunter2@db:27017 failed`,
		Error:   `connecting to mongodb://admin:hunter2@db:27017 failed`,
	})

	event := received.wait(t)
	for _, field := range []string{event.Summary, event.Error} {
		if strings.Contains(field, "hunter2") {
			t.Fatalf("a password was sent to the webhook: %s", field)
		}
	}
}

// Most installations want silence on success. Paging someone every night for
// good news is how people learn to ignore the alerts.
func TestSuccessIsSilentUnlessAskedFor(t *testing.T) {
	received, server := receiver(t)
	defer server.Close()

	New(Settings{WebhookURL: server.URL}, discard()).
		Notify(t.Context(), Event{Kind: KindRunSucceeded, ConfigName: "quiet"})

	if received.count() != 0 {
		t.Fatal("a successful run notified without being asked to")
	}

	New(Settings{WebhookURL: server.URL, OnSuccess: true}, discard()).
		Notify(t.Context(), Event{Kind: KindRunSucceeded, ConfigName: "loud"})

	if event := received.wait(t); event.ConfigName != "loud" {
		t.Errorf("config is %q, want %q", event.ConfigName, "loud")
	}
}

// No webhook configured must be a working no-op, not a nil dereference in the
// middle of reporting a failure.
func TestAnUnconfiguredNotifierDoesNothingRatherThanPanicking(t *testing.T) {
	notifier := New(Settings{WebhookURL: "   "}, discard())
	if notifier != nil {
		t.Fatal("expected no notifier when no webhook is set")
	}
	notifier.Notify(t.Context(), Event{Kind: KindRunFailed}) // must not panic
}

// A notification is a courtesy. If it could fail a run, the system meant to
// report outages would be able to cause them.
func TestADeadWebhookDoesNotBlockOrFail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		New(Settings{WebhookURL: server.URL}, discard()).Notify(t.Context(), Event{Kind: KindRunFailed})
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("notifying a failing webhook did not return")
	}
}

// A run cancelled by shutdown is exactly when the notification matters most, so
// delivery must not inherit the cancelled context.
func TestACancelledRunStillNotifies(t *testing.T) {
	received, server := receiver(t)
	defer server.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	New(Settings{WebhookURL: server.URL}, discard()).
		Notify(ctx, Event{Kind: KindRunFailed, ConfigName: "interrupted"})

	if event := received.wait(t); event.ConfigName != "interrupted" {
		t.Errorf("config is %q", event.ConfigName)
	}
}

func TestDescribeReadsAsASentence(t *testing.T) {
	got := Describe("orders", "success", 500, 500, nil)
	for _, want := range []string{"orders", "500"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q does not mention %q", got, want)
		}
	}
	if !strings.Contains(Describe("orders", "success", 500, 0, nil), "archived 500 documents") {
		t.Error("a run that deleted nothing should not claim it deleted something")
	}
}

// --- helpers ---

type inbox struct {
	mu     sync.Mutex
	events []Event
	got    chan struct{}
}

func (i *inbox) add(event Event) {
	i.mu.Lock()
	i.events = append(i.events, event)
	i.mu.Unlock()
	select {
	case i.got <- struct{}{}:
	default:
	}
}

func (i *inbox) count() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.events)
}

func (i *inbox) wait(t *testing.T) Event {
	t.Helper()
	select {
	case <-i.got:
	case <-time.After(5 * time.Second):
		t.Fatal("no notification arrived")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.events[len(i.events)-1]
}

func receiver(t *testing.T) (*inbox, *httptest.Server) {
	t.Helper()
	box := &inbox{got: make(chan struct{}, 16)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var event Event
		if err := json.Unmarshal(body, &event); err != nil {
			t.Errorf("webhook body is not valid JSON: %v", err)
		}
		box.add(event)
		w.WriteHeader(http.StatusOK)
	}))
	return box, server
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
