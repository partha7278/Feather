package logging

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
// Init binds its handler to os.Stdout at call time, so the redirect has to be
// in place before Init runs.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	previous := slog.Default()
	os.Stdout = w
	defer func() {
		os.Stdout = original
		slog.SetDefault(previous)
	}()

	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	return <-done
}

// Values in a log line come from places an attacker can reach: a collection
// name, a webhook response, an HTTP header. A newline in one must not let it
// forge a second, trustworthy-looking log record.
func TestALoggedValueCannotForgeASecondRecord(t *testing.T) {
	forged := "innocent\n{\"time\":\"2026-01-01T00:00:00Z\",\"level\":\"INFO\",\"msg\":\"admin login\",\"by\":\"attacker\"}\r\nlevel=INFO msg=\"admin login\""

	for _, jsonOutput := range []bool{true, false} {
		name := "text"
		if jsonOutput {
			name = "json"
		}
		t.Run(name, func(t *testing.T) {
			out := captureStdout(t, func() {
				log := Init("info", jsonOutput)
				log.Info("request", "path", forged)
				log.Info(forged)
			})
			lines := nonEmptyLines(out)
			if len(lines) != 2 {
				t.Fatalf("two records produced %d lines:\n%s", len(lines), out)
			}
			if jsonOutput {
				for _, line := range lines {
					var record map[string]any
					if err := json.Unmarshal([]byte(line), &record); err != nil {
						t.Fatalf("a record is not one JSON object: %q", line)
					}
					if record["msg"] == "admin login" {
						t.Fatalf("a forged record was emitted: %s", line)
					}
				}
			}
		})
	}
}

// A hostile key is as dangerous as a hostile value.
func TestALoggedKeyCannotForgeARecord(t *testing.T) {
	out := captureStdout(t, func() {
		Init("info", true).Info("x", "k\n{\"msg\":\"forged\"}", "v")
	})
	if lines := nonEmptyLines(out); len(lines) != 1 {
		t.Fatalf("one record produced %d lines:\n%s", len(lines), out)
	}
}

// Debug output is where request bodies and driver chatter end up. An
// unrecognised or hostile LOG_LEVEL must fall back to info, never open it up.
func TestAnUnrecognisedLevelDoesNotEnableDebug(t *testing.T) {
	for _, level := range []string{"", "verbose", "trace", "all", "-4", "DEBUG ", "debug\n", "info;debug"} {
		got := parseLevel(level)
		want := slog.LevelInfo
		if strings.TrimSpace(strings.ToLower(level)) == "debug" {
			want = slog.LevelDebug
		}
		if got != want {
			t.Errorf("parseLevel(%q) = %v, want %v", level, got, want)
		}
	}

	out := captureStdout(t, func() {
		log := Init("verbose", true)
		log.Debug("debug-only detail")
	})
	if strings.Contains(out, "debug-only detail") {
		t.Error("an unknown level let a debug record through")
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	scanner := bufio.NewScanner(strings.NewReader(s))
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			out = append(out, scanner.Text())
		}
	}
	return out
}
