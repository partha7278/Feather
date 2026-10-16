// Package notify tells somebody when a run fails.
//
// Without this, the only way to learn that a nightly archive has been failing
// for a week is to open the control panel and look — which is exactly what
// nobody does while everything appears to be fine. For a job that runs
// unattended and deletes production data, silence has to be distinguishable
// from success.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/unifers/feather/internal/config"
)

// Event is something worth telling an operator about.
type Event struct {
	Kind       Kind      `json:"kind"`
	Instance   string    `json:"instance"`
	Host       string    `json:"host"`
	ConfigName string    `json:"config,omitempty"`
	RunID      string    `json:"runId,omitempty"`
	Status     string    `json:"status,omitempty"`
	At         time.Time `json:"at"`

	DocsArchived int64 `json:"docsArchived,omitempty"`
	DocsDeleted  int64 `json:"docsDeleted,omitempty"`

	// Summary is one sentence, written for a human reading a chat message.
	Summary string `json:"summary"`
	// Error is the failure, already redacted.
	Error string `json:"error,omitempty"`
}

// Kind names what happened.
type Kind string

const (
	// KindRunFailed is a run that ended in failure or got part way.
	KindRunFailed Kind = "run.failed"
	// KindVerifyFailed is an archive that could not be read back.
	KindVerifyFailed Kind = "archive.verify_failed"
	// KindRunSucceeded is sent only when asked for. Most installations want
	// silence on success; some want a heartbeat.
	KindRunSucceeded Kind = "run.succeeded"
)

// Notifier delivers events. A nil *Webhook is a working no-op, so callers never
// have to check whether notifications are configured.
type Notifier interface {
	Notify(ctx context.Context, event Event)
}

// Settings is what an operator configures.
type Settings struct {
	// WebhookURL receives a JSON POST per event. Empty disables notifications.
	WebhookURL string
	// OnSuccess sends an event for successful runs too.
	OnSuccess bool
}

// Webhook posts events as JSON.
type Webhook struct {
	url       string
	onSuccess bool
	client    *http.Client
	log       *slog.Logger
}

// New builds a notifier from settings. It returns nil when nothing is
// configured, which Notify treats as "do nothing".
func New(settings Settings, log *slog.Logger) *Webhook {
	if strings.TrimSpace(settings.WebhookURL) == "" {
		return nil
	}
	return &Webhook{
		url:       strings.TrimSpace(settings.WebhookURL),
		onSuccess: settings.OnSuccess,
		// Short: a notification is a courtesy, and a slow endpoint must not
		// hold up the run that is trying to report on itself.
		client: &http.Client{Timeout: 10 * time.Second},
		log:    log,
	}
}

// Notify delivers one event, best effort.
//
// A failure to notify is logged and swallowed. The alternative — failing a run
// because the chat webhook was down — would mean the notification system could
// cause the outages it exists to report.
func (w *Webhook) Notify(ctx context.Context, event Event) {
	if w == nil {
		return
	}
	if event.Kind == KindRunSucceeded && !w.onSuccess {
		return
	}

	// Redacted here rather than at every call site, because this is the last
	// place the text passes through before it leaves the machine.
	event.Error = config.RedactSecrets(event.Error)
	event.Summary = config.RedactSecrets(event.Summary)

	body, err := json.Marshal(event)
	if err != nil {
		w.log.Warn("could not encode notification", "error", err)
		return
	}

	// Detached from the run's context on purpose: a run cancelled by shutdown
	// is precisely when the notification matters most, and inheriting its
	// cancelled context would drop it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		w.log.Warn("could not build notification request", "url_host", hostOf(w.url), "error", w.safeError(err))
		return
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "feather")

	response, err := w.client.Do(request)
	if err != nil {
		w.log.Warn("could not deliver notification", "kind", event.Kind, "url_host", hostOf(w.url), "error", w.safeError(err))
		return
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode >= 300 {
		w.log.Warn("notification was rejected",
			"kind", event.Kind, "status", response.StatusCode, "url_host", hostOf(w.url))
		return
	}
	w.log.Debug("notification delivered", "kind", event.Kind, "config", event.ConfigName)
}

// Describe renders the one-sentence summary for a finished run.
func Describe(configName, status string, archived, deleted int64, err error) string {
	if err != nil {
		return fmt.Sprintf("feather: retention config %q ended %s — %s", configName, status, err.Error())
	}
	if deleted > 0 {
		return fmt.Sprintf("feather: retention config %q archived %d documents and deleted %d from MongoDB",
			configName, archived, deleted)
	}
	return fmt.Sprintf("feather: retention config %q archived %d documents", configName, archived)
}

// hostOf names the webhook endpoint by host alone. A chat webhook URL is a
// bearer credential — Slack, Discord and Teams carry the secret in the path or
// the query, and some operators put one in the userinfo — so nothing but the
// host may be logged.
func hostOf(raw string) string {
	rest := raw
	if i := strings.Index(raw, "://"); i >= 0 {
		rest = raw[i+3:]
	}
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		rest = rest[:j]
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	return rest
}

// safeError renders a delivery error without the webhook URL. Go's transport
// errors quote the whole URL, path and query included; the *url.Error is
// rebuilt around the host, and any other mention of the URL is cut out too.
func (w *Webhook) safeError(err error) string {
	host := hostOf(w.url)
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = &url.Error{Op: urlErr.Op, URL: host, Err: urlErr.Err}
	}
	message := err.Error()
	if w.url != "" {
		message = strings.ReplaceAll(message, w.url, host)
	}
	return config.RedactSecrets(message)
}
