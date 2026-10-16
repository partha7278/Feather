package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewRejectsAKeyOfTheWrongLength(t *testing.T) {
	for _, size := range []int{0, 16, 24, 31, 33, 64} {
		box, err := New(make([]byte, size), "test")
		if err == nil {
			t.Errorf("a %d-byte key was accepted", size)
			continue
		}
		if box != nil {
			t.Errorf("a %d-byte key returned a box alongside the error", size)
		}
	}

	box, err := New(make([]byte, KeySize), "the source")
	if err != nil {
		t.Fatal(err)
	}
	if box.Source != "the source" {
		t.Errorf("source %q was not recorded", box.Source)
	}
}

func TestResolveRequiresAPathWhenTheEnvironmentHasNoKey(t *testing.T) {
	for _, env := range []string{"", "   ", "\n\t"} {
		_, err := Resolve(env, "")
		if err == nil || !strings.Contains(err.Error(), "no master key path") {
			t.Errorf("env %q with no path: got %v, want a missing-path error", env, err)
		}
	}
}

// A key pasted into an env file usually brings a newline with it.
func TestResolveTrimsWhitespaceAroundTheEnvironmentKey(t *testing.T) {
	key, err := GenerateKeyHex()
	if err != nil {
		t.Fatal(err)
	}
	box, err := Resolve("  "+key+"\n", "")
	if err != nil {
		t.Fatalf("a padded key was rejected: %v", err)
	}
	if box.Source != "FEATHER_MASTER_KEY" {
		t.Errorf("source %q, want the environment", box.Source)
	}
}

func TestResolveExplainsAMalformedEnvironmentKey(t *testing.T) {
	cases := []struct {
		name, value, want string
	}{
		{"not hex", strings.Repeat("z", KeySize*2), "hex characters"},
		{"too short", strings.Repeat("ab", KeySize-1), "got 31"},
		{"too long", strings.Repeat("ab", KeySize+1), "got 33"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Resolve(tc.value, filepath.Join(t.TempDir(), "master.key"))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.HasPrefix(err.Error(), "FEATHER_MASTER_KEY: ") || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the variable and the problem (%q)", err, tc.want)
			}
		})
	}
}

// A key file written by hand has a trailing newline; reading it must not fail
// on that, and the box must say where its key came from.
func TestResolveReadsAnExistingKeyFile(t *testing.T) {
	key, err := GenerateKeyHex()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, []byte("\n"+key+"\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	fromFile, err := Resolve("", path)
	if err != nil {
		t.Fatal(err)
	}
	if fromFile.Source != path {
		t.Errorf("source %q, want the file path", fromFile.Source)
	}

	// The same key supplied through the environment must open what the file's
	// box sealed: they are one key, not two that happen to share a source.
	fromEnv, err := Resolve(key, "")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := fromFile.Seal("secret")
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := fromEnv.Open(sealed); err != nil || opened != "secret" {
		t.Errorf("the file and environment keys disagree: %q, %v", opened, err)
	}
}

func TestResolveRejectsACorruptKeyFile(t *testing.T) {
	cases := []struct {
		name, content, want string
	}{
		{"empty", "", "got 0"},
		{"not hex", "this is not a key", "hex characters"},
		{"half a key", strings.Repeat("ab", KeySize/2), "got 16"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "master.key")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err := Resolve("", path)
			if err == nil {
				t.Fatal("a corrupt key file was accepted")
			}
			if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the file and the problem (%q)", err, tc.want)
			}

			// The corrupt file must be left for the operator to inspect, not
			// replaced by a fresh key that cannot open anything already sealed.
			data, err := os.ReadFile(path)
			if err != nil || string(data) != tc.content {
				t.Errorf("the key file was changed: %q, %v", data, err)
			}
		})
	}
}

// The permission check applies to group as well as other.
func TestResolveRejectsAGroupReadableKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	key, err := GenerateKeyHex()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}

	_, err = Resolve("", path)
	if err == nil {
		t.Fatal("a group-readable key file was accepted")
	}
	if !strings.Contains(err.Error(), "0640") || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("error does not say what is wrong or how to fix it: %v", err)
	}
}

// Anything other than "does not exist" must not be treated as a fresh install:
// generating a new key over an unreadable one would orphan every secret.
func TestResolveRefusesAKeyPathItCannotRead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "master.key")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	_, err := Resolve("", dir)
	if err == nil {
		t.Fatal("a directory was accepted as a key file")
	}
	if !strings.Contains(err.Error(), "reading master key") {
		t.Errorf("error does not explain the failure: %v", err)
	}
}

func TestResolveReportsAKeyDirectoryItCannotCreate(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Resolve("", filepath.Join(blocker, "master.key"))
	if err == nil {
		t.Fatal("a key was generated under a path whose parent is a file")
	}
	if !strings.Contains(err.Error(), "not-a-dir") {
		t.Errorf("error does not name the path: %v", err)
	}
}

func TestResolveReportsAKeyFileItCannotWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	path := filepath.Join(dir, "master.key")
	_, err := Resolve("", path)
	if err == nil {
		t.Fatal("a key was reported generated in a read-only directory")
	}
	if !strings.Contains(err.Error(), "writing master key") {
		t.Errorf("error does not explain the failure: %v", err)
	}
}

func TestResolveSaysWhenItGeneratedTheKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")

	box, err := Resolve("", path)
	if err != nil {
		t.Fatal(err)
	}
	if box.Source != path+" (newly generated)" {
		t.Errorf("source %q does not say the key is new", box.Source)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); len(got) != KeySize*2 {
		t.Errorf("key file holds %d characters, want %d hex characters", len(got), KeySize*2)
	}
}

func TestGenerateKeyHexProducesDistinctUsableKeys(t *testing.T) {
	seen := map[string]bool{}
	for range 20 {
		key, err := GenerateKeyHex()
		if err != nil {
			t.Fatal(err)
		}
		if len(key) != KeySize*2 {
			t.Fatalf("key %q is %d characters", key, len(key))
		}
		if seen[key] {
			t.Fatal("the same key was generated twice")
		}
		seen[key] = true
		if _, err := Resolve(key, ""); err != nil {
			t.Fatalf("a generated key was rejected: %v", err)
		}
	}
}
