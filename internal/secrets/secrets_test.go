package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	box := testBox(t)

	for _, plaintext := range []string{
		"mongodb://user:p@ssw0rd@127.0.0.1:27017/?replicaSet=rs0",
		"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		strings.Repeat("long", 5000),
		"unicode ✓ ünïcödé",
	} {
		sealed, err := box.Seal(plaintext)
		if err != nil {
			t.Fatalf("sealing: %v", err)
		}
		if strings.Contains(string(sealed), plaintext) {
			t.Fatal("sealed value still contains the plaintext")
		}

		opened, err := box.Open(sealed)
		if err != nil {
			t.Fatalf("opening: %v", err)
		}
		if opened != plaintext {
			t.Fatalf("round trip changed the value: got %q", opened)
		}
	}
}

// An empty secret must stay empty rather than becoming an encrypted blob, so
// "no session token" and "an empty session token" do not become the same thing.
func TestSealEmptyStaysEmpty(t *testing.T) {
	box := testBox(t)

	sealed, err := box.Seal("")
	if err != nil {
		t.Fatal(err)
	}
	if sealed != nil {
		t.Fatalf("expected nil for an empty secret, got %v", sealed)
	}

	opened, err := box.Open(nil)
	if err != nil || opened != "" {
		t.Fatalf("opening nothing: got %q, %v", opened, err)
	}
}

// Sealing the same value twice must not produce the same bytes: a fresh nonce
// each time is what stops an observer inferring that two accounts share a key.
func TestSealIsNotDeterministic(t *testing.T) {
	box := testBox(t)

	first, err := box.Seal("same secret")
	if err != nil {
		t.Fatal(err)
	}
	second, err := box.Seal("same secret")
	if err != nil {
		t.Fatal(err)
	}
	if string(first) == string(second) {
		t.Fatal("two seals of the same value produced identical ciphertext")
	}
}

func TestOpenWithWrongKeyFails(t *testing.T) {
	sealed, err := testBox(t).Seal("secret")
	if err != nil {
		t.Fatal(err)
	}

	other, err := New(make([]byte, KeySize), "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("expected decryption with the wrong key to fail")
	}
}

func TestOpenRejectsTruncatedValue(t *testing.T) {
	if _, err := testBox(t).Open([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected a truncated value to be rejected")
	}
}

// A fresh install has to be able to start, so the key is generated on first
// use — and must be reused, not regenerated, on the next start.
func TestResolveGeneratesThenReusesKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "master.key")

	first, err := Resolve("", path)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	sealed, err := first.Seal("secret")
	if err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("key file is mode %04o, want 0600", mode)
	}

	second, err := Resolve("", path)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	opened, err := second.Open(sealed)
	if err != nil || opened != "secret" {
		t.Fatalf("key was not reused: got %q, %v", opened, err)
	}
}

func TestResolvePrefersEnvironmentKey(t *testing.T) {
	key, err := GenerateKeyHex()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "master.key")

	box, err := Resolve(key, path)
	if err != nil {
		t.Fatal(err)
	}
	if box.Source != "FEATHER_MASTER_KEY" {
		t.Fatalf("source is %q, want the environment", box.Source)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a key file was written even though the environment supplied a key")
	}
}

func TestResolveRejectsMalformedEnvironmentKey(t *testing.T) {
	for _, bad := range []string{"not-hex", "abcd"} {
		if _, err := Resolve(bad, filepath.Join(t.TempDir(), "master.key")); err == nil {
			t.Fatalf("expected %q to be rejected", bad)
		}
	}
}

// A key file anyone can read is not protecting the database next to it.
func TestResolveRejectsWorldReadableKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	key, err := GenerateKeyHex()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(key), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Resolve("", path); err == nil {
		t.Fatal("expected a world-readable key file to be refused")
	}
}

func testBox(t *testing.T) *Box {
	t.Helper()

	key, err := GenerateKeyHex()
	if err != nil {
		t.Fatal(err)
	}
	box, err := Resolve(key, "")
	if err != nil {
		t.Fatal(err)
	}
	return box
}
