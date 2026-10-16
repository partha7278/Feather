package secrets

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

func testKeyHex(t *testing.T) string {
	t.Helper()
	k, err := GenerateKeyHex()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// A malformed key is still most of a key. An error that quotes it back puts the
// master key into the log, the systemd journal, or a screenshot of a crash.
func TestResolveErrorsNeverQuoteTheKeyMaterial(t *testing.T) {
	good := testKeyHex(t)
	cases := map[string]string{
		"one character short":       good[:63],
		"one character long":        good + "a",
		"non-hex character":         good[:62] + "zz",
		"a sixteen-byte key":        good[:32],
		"a key with an inner space": good[:32] + " " + good[32:],
	}
	for name, bad := range cases {
		t.Run("environment: "+name, func(t *testing.T) {
			_, err := Resolve(bad, "")
			if err == nil {
				t.Fatal("accepted a malformed key")
			}
			assertNoKeyMaterial(t, err.Error(), bad)
		})
		t.Run("file: "+name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "master.key")
			if err := os.WriteFile(path, []byte(bad+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Resolve("", path)
			if err == nil {
				t.Fatal("accepted a malformed key file")
			}
			assertNoKeyMaterial(t, err.Error(), bad)
		})
	}
}

func assertNoKeyMaterial(t *testing.T, message, key string) {
	t.Helper()
	compact := strings.ReplaceAll(key, " ", "")
	for _, chunk := range []string{compact, compact[:min(16, len(compact))], compact[len(compact)-min(16, len(compact)):]} {
		if strings.Contains(message, chunk) {
			t.Errorf("error quotes key material %q: %s", chunk, message)
		}
	}
}

// Even a valid key in a file that is too widely readable must be refused
// without being repeated in the refusal.
func TestPermissionRefusalDoesNotQuoteTheKey(t *testing.T) {
	key := testKeyHex(t)
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, []byte(key), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Resolve("", path)
	if err == nil {
		t.Fatal("a world-readable key file was accepted")
	}
	assertNoKeyMaterial(t, err.Error(), key)
}

// Every permission bit for group or other is a reason to refuse — including
// write and execute, since a group-writable key can be swapped for one an
// attacker knows.
func TestResolveRefusesAnyGroupOrOtherPermission(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o620, 0o610, 0o604, 0o602, 0o601, 0o660, 0o666, 0o777} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "master.key")
			if err := os.WriteFile(path, []byte(testKeyHex(t)), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := Resolve("", path); err == nil {
				t.Fatalf("a key file with mode %04o was accepted", mode)
			}
		})
	}
	for _, mode := range []os.FileMode{0o600, 0o400} {
		path := filepath.Join(t.TempDir(), "master.key")
		if err := os.WriteFile(path, []byte(testKeyHex(t)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := Resolve("", path); err != nil {
			t.Errorf("a key file with mode %04o was refused: %v", mode, err)
		}
	}
}

// A symlink to a world-readable key is the same exposure as the file itself.
// The permission check must judge the file the key is read from, not the link.
func TestResolveJudgesPermissionsThroughASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "shared.key")
	if err := os.WriteFile(target, []byte(testKeyHex(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "master.key")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve("", link); err == nil {
		t.Fatal("a symlink to a world-readable key was accepted")
	}
}

// A freshly generated key and the directory made for it are private from the
// moment they exist; there is no window in which they are readable by others.
func TestAGeneratedKeyAndItsDirectoryArePrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "master.key")
	if _, err := Resolve("", path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("generated key file has mode %04o", perm)
	}
	for _, dir := range []string{filepath.Dir(path), filepath.Dir(filepath.Dir(path))} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("key directory %s has mode %04o", dir, perm)
		}
	}
}

// A symlink planted at the key path — pointing at a file the attacker will
// later read — must not cause the new master key to be written through it.
func TestResolveDoesNotWriteANewKeyThroughAPlantedSymlink(t *testing.T) {
	if os.Getenv("FEATHER_SECRETS_SYMLINK_CHILD") == "1" {
		// Bound the stack so unbounded recursion fails in milliseconds.
		debug.SetMaxStack(8 << 20)
		dir := os.Getenv("FEATHER_SECRETS_SYMLINK_DIR")
		_, err := Resolve("", filepath.Join(dir, "master.key"))
		if err == nil {
			fmt.Println("RESOLVED")
		} else {
			fmt.Println("ERROR:", err)
		}
		return
	}

	dir := t.TempDir()
	trap := filepath.Join(dir, "attacker-readable")
	if err := os.Symlink(trap, filepath.Join(dir, "master.key")); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestResolveDoesNotWriteANewKeyThroughAPlantedSymlink$", "-test.count=1")
	cmd.Env = append(os.Environ(), "FEATHER_SECRETS_SYMLINK_CHILD=1", "FEATHER_SECRETS_SYMLINK_DIR="+dir)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	runErr := cmd.Run()

	if _, err := os.Stat(trap); err == nil {
		t.Fatal("the master key was written through a planted symlink")
	}
	// The dangling link must produce a clean refusal, not a crash or a hang:
	// Resolve must terminate and the process must survive it.
	if runErr != nil || !strings.Contains(out.String(), "ERROR:") {
		first := out.String()
		if len(first) > 600 {
			first = first[:600]
		}
		t.Fatalf("Resolve did not return an error for a dangling symlink at the key path (exit: %v):\n%s", runErr, first)
	}
}

// Sealed values are authenticated: a flipped bit anywhere is refused rather
// than decrypted to something subtly different.
func TestTamperedCiphertextIsRefusedEverywhere(t *testing.T) {
	box, err := New(bytes.Repeat([]byte{7}, KeySize), "test")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Seal("mongodb://app:hunter2@db")
	if err != nil {
		t.Fatal(err)
	}
	for i := range sealed {
		tampered := bytes.Clone(sealed)
		tampered[i] ^= 0x01
		if got, err := box.Open(tampered); !errors.Is(err, ErrKeyMismatch) {
			t.Fatalf("flipping byte %d gave %q, %v", i, got, err)
		}
	}
	if _, err := box.Open(sealed[:len(sealed)-1]); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("a truncated tag was accepted: %v", err)
	}
	if _, err := box.Open(append(bytes.Clone(sealed), 0)); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("an extended ciphertext was accepted: %v", err)
	}
}

// The ciphertext must not carry the plaintext, nor any long run of it.
func TestSealedValuesDoNotContainThePlaintext(t *testing.T) {
	box, err := New(bytes.Repeat([]byte{9}, KeySize), "test")
	if err != nil {
		t.Fatal(err)
	}
	plain := "AKIA-very-secret-access-key-material"
	sealed, err := box.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(plain[:8])) || strings.Contains(hex.EncodeToString(sealed), hex.EncodeToString([]byte(plain[:8]))) {
		t.Fatal("the sealed value contains plaintext")
	}
}

// The error a wrong key produces is a fixed sentence. It must not include the
// ciphertext, the plaintext, or anything derived from the key.
func TestOpenErrorsAreOpaque(t *testing.T) {
	a, _ := New(bytes.Repeat([]byte{1}, KeySize), "a")
	b, _ := New(bytes.Repeat([]byte{2}, KeySize), "b")
	sealed, err := a.Seal("plaintext-secret")
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Open(sealed)
	if !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("got %v", err)
	}
	if err.Error() != ErrKeyMismatch.Error() {
		t.Errorf("the mismatch error carries extra detail: %s", err)
	}
	if strings.Contains(err.Error(), "plaintext-secret") || strings.Contains(err.Error(), hex.EncodeToString(sealed[:8])) {
		t.Errorf("the mismatch error leaks data: %s", err)
	}
}

// A Box ends up in structs that get printed. None of the ways Go renders a
// value may reveal the key, and Source must name where the key came from, never
// the key itself.
func TestABoxNeverPrintsItsKey(t *testing.T) {
	key := testKeyHex(t)
	box, err := Resolve(key, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(box.Source, key) || box.Source != "FEATHER_MASTER_KEY" {
		t.Errorf("Source is %q", box.Source)
	}
	raw, _ := hex.DecodeString(key)
	encoded, _ := json.Marshal(box)
	for _, rendering := range []string{
		fmt.Sprintf("%v", box), fmt.Sprintf("%+v", box), fmt.Sprintf("%#v", box),
		fmt.Sprintf("%v", *box), fmt.Sprintf("%+v", *box), string(encoded),
	} {
		if strings.Contains(rendering, key) || strings.Contains(rendering, string(raw)) {
			t.Errorf("a rendering of the box contains the key: %s", rendering)
		}
	}
}

// The environment key wins over the file, and when it does the file must not be
// read, created or overwritten — an operator moving to an injected key should
// not find a fresh key file appear beside the database.
func TestAnEnvironmentKeyLeavesTheKeyPathUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	if _, err := Resolve(testKeyHex(t), path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("a key file was created although the environment supplied the key: %v", err)
	}
}
