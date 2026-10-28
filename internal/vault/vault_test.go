package vault

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	v, err := Open(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	for _, plain := range []string{"", "hunter2", "p@ss\"word' with spaces", strings.Repeat("x", 4096)} {
		enc, err := v.Encrypt(plain)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(enc, plain) && plain != "" {
			t.Fatalf("ciphertext contains plaintext")
		}
		got, err := v.Decrypt(enc)
		if err != nil {
			t.Fatal(err)
		}
		if got != plain {
			t.Fatalf("got %q want %q", got, plain)
		}
	}
}

func TestEncryptIsNondeterministic(t *testing.T) {
	v, _ := Open(filepath.Join(t.TempDir(), "k"))
	a, _ := v.Encrypt("same")
	b, _ := v.Encrypt("same")
	if a == b {
		t.Fatal("two encryptions of the same value must differ (random nonce)")
	}
}

func TestTamperDetected(t *testing.T) {
	v, _ := Open(filepath.Join(t.TempDir(), "k"))
	enc, _ := v.Encrypt("secret")
	b := []byte(enc)
	b[len(b)-1] ^= 0x01
	if _, err := v.Decrypt(string(b)); err == nil {
		t.Fatal("tampered ciphertext decrypted without error")
	}
	if _, err := v.Decrypt("garbage"); err == nil {
		t.Fatal("garbage decrypted without error")
	}
}

func TestDifferentKeyFails(t *testing.T) {
	v1, _ := Open(filepath.Join(t.TempDir(), "k"))
	v2, _ := Open(filepath.Join(t.TempDir(), "k"))
	enc, _ := v1.Encrypt("secret")
	if _, err := v2.Decrypt(enc); err == nil {
		t.Fatal("decrypted with the wrong key")
	}
}

func TestKeyPersistsAndIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	v1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := v1.Encrypt("secret")

	v2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := v2.Decrypt(enc)
	if err != nil || got != "secret" {
		t.Fatalf("reopened vault could not decrypt: %v", err)
	}

	if runtime.GOOS != "windows" {
		st, _ := os.Stat(path)
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("key file mode = %o, want 0600", st.Mode().Perm())
		}
	}
}

func TestRejectsBadKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	os.WriteFile(path, []byte("not hex"), 0o600)
	if _, err := Open(path); err == nil {
		t.Fatal("expected error for corrupt key file")
	}
}
