package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestResolveTimeTokens(t *testing.T) {
	now := time.Date(2026, 8, 12, 15, 30, 0, 0, time.UTC)

	tests := []struct {
		name  string
		in    string
		want  string
		error string
	}{
		{
			name: "relative days",
			in:   `{"createdAt":{"$lt":{"$date":"{{now-90d}}"}}}`,
			want: `{"createdAt":{"$lt":{"$date":"2026-05-14T15:30:00.000Z"}}}`,
		},
		{
			name: "bare now",
			in:   `{"at":{"$date":"{{now}}"}}`,
			want: `{"at":{"$date":"2026-08-12T15:30:00.000Z"}}`,
		},
		{
			name: "hours and spaces",
			in:   `{"at":{"$date":"{{ now - 6h }}"}}`,
			want: `{"at":{"$date":"2026-08-12T09:30:00.000Z"}}`,
		},
		{
			name: "future offset",
			in:   `{"at":{"$date":"{{now+1d}}"}}`,
			want: `{"at":{"$date":"2026-08-13T15:30:00.000Z"}}`,
		},
		{
			name: "no tokens is untouched",
			in:   `{"status":"completed"}`,
			want: `{"status":"completed"}`,
		},
		{
			name:  "a typo must not reach mongo as a literal",
			in:    `{"at":{"$date":"{{niw-90d}}"}}`,
			error: "unrecognised template token",
		},
		{
			name:  "unknown unit is rejected",
			in:    `{"at":{"$date":"{{now-90years}}"}}`,
			error: "cannot parse duration",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveTimeTokens(json.RawMessage(tc.in), now)

			if tc.error != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got none (result %s)", tc.error, got)
				}
				if !strings.Contains(err.Error(), tc.error) {
					t.Fatalf("expected error containing %q, got %v", tc.error, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestRenderKey(t *testing.T) {
	key := RenderKey("feather/{db}/{collection}/{yyyy}/{MM}/{dd}/{config}-{runId}-{seq}{ext}", KeyVars{
		Database:   "shop",
		Collection: "orders",
		Config:     "orders-archive",
		RunID:      "abc-123",
		Seq:        7,
		Extension:  ".ndjson.gz",
		At:         time.Date(2026, 8, 12, 2, 30, 0, 0, time.UTC),
	})

	want := "feather/shop/orders/2026/08/12/orders-archive-abc-123-00007.ndjson.gz"
	if key != want {
		t.Errorf("got  %s\nwant %s", key, want)
	}
}

func TestParseDuration(t *testing.T) {
	tests := map[string]time.Duration{
		"30m": 30 * time.Minute,
		"6h":  6 * time.Hour,
		"1d":  24 * time.Hour,
		"2d":  48 * time.Hour,
	}
	for in, want := range tests {
		got, err := ParseDuration(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != want {
			t.Errorf("%s: got %v want %v", in, got, want)
		}
	}

	if _, err := ParseDuration("2 fortnights"); err == nil {
		t.Error("expected an error for a nonsense duration")
	}
}

func TestByteSize(t *testing.T) {
	tests := map[string]int64{
		`"256MB"`: 256 << 20,
		`"1GB"`:   1 << 30,
		`"512KB"`: 512 << 10,
		`"64MiB"`: 64 << 20,
		`1048576`: 1 << 20,
	}
	for in, want := range tests {
		var got ByteSize
		if err := got.UnmarshalJSON([]byte(in)); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if int64(got) != want {
			t.Errorf("%s: got %d want %d", in, got, want)
		}
	}
}
