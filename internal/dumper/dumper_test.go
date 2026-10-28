package dumper

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestArgsAreReadOnly(t *testing.T) {
	args := Args("/tmp/x.cnf", "shop")
	joined := strings.Join(args, " ")
	if !strings.HasPrefix(args[0], "--defaults-extra-file=") {
		t.Fatal("--defaults-extra-file must be the first argument")
	}
	for _, must := range []string{"--single-transaction", "--skip-lock-tables"} {
		if !strings.Contains(joined, must) {
			t.Errorf("missing %s", must)
		}
	}
	for _, never := range []string{"--lock-tables", "--lock-all-tables", "--flush", "-p", "--password", "--add-drop-database", "--databases"} {
		for _, a := range args {
			if a == never || strings.HasPrefix(a, never+"=") {
				t.Errorf("forbidden flag %s present", never)
			}
		}
	}
	if args[len(args)-1] != "shop" {
		t.Error("database must be the last argument")
	}
}

func TestOptionFileNeverOnCommandLine(t *testing.T) {
	tgt := Target{Host: "db.local", Port: 3306, Database: "shop", Username: "ro", Password: `p"w\d`}
	path, err := writeOptionFile(t.TempDir(), tgt)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	for _, a := range Args(path, tgt.Database) {
		if strings.Contains(a, tgt.Password) {
			t.Fatal("password leaked into arguments")
		}
	}

	data, _ := os.ReadFile(path)
	content := string(data)
	for _, want := range []string{"[client]", `host="db.local"`, "port=3306", `user="ro"`, `password="p\"w\\d"`, "skip-ssl"} {
		if !strings.Contains(content, want) {
			t.Errorf("option file missing %q:\n%s", want, content)
		}
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(path)
		if st.Mode().Perm() != 0o600 {
			t.Errorf("option file mode %o, want 0600", st.Mode().Perm())
		}
	}
}

func TestOptionFileTLS(t *testing.T) {
	tgt := Target{Host: "h", Port: 3306, Database: "d", Username: "u", Password: "p", TLS: true}
	path, _ := writeOptionFile(t.TempDir(), tgt)
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "\nssl\n") || strings.Contains(string(data), "skip-ssl\n") {
		t.Errorf("TLS option file wrong:\n%s", data)
	}
}

func TestValidateRejectsInjection(t *testing.T) {
	ok := Target{Host: "h", Port: 3306, Database: "d", Username: "u", Password: "p"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []Target{
		{Host: "", Port: 3306, Database: "d", Username: "u"},
		{Host: "h", Port: 0, Database: "d", Username: "u"},
		{Host: "h", Port: 70000, Database: "d", Username: "u"},
		{Host: "h", Port: 3306, Database: "--all-databases", Username: "u"},
		{Host: "h", Port: 3306, Database: "d\nx", Username: "u"},
		{Host: "h", Port: 3306, Database: "d", Username: ""},
		{Host: "h", Port: 3306, Database: "d", Username: "u\n[mysqld]"},
		{Host: "h", Port: 3306, Database: "d", Username: "u", Password: "p\nskip-ssl"},
	}
	for _, b := range bad {
		if err := b.Validate(); err == nil {
			t.Errorf("%+v should be rejected", b)
		}
	}
}

func TestDumpFailureLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.sql")
	// Pre-existing good backup must survive a failed run.
	os.WriteFile(dest, []byte("good"), 0o640)

	d := &Dumper{Binary: "definitely-not-a-real-binary-xyz", TempDir: dir}
	tgt := Target{Host: "h", Port: 3306, Database: "d", Username: "u", Password: "secretpw"}
	_, err := d.Dump(context.Background(), tgt, dest, false)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "secretpw") {
		t.Fatal("password leaked into error")
	}
	if _, err := os.Stat(dest + ".partial"); err == nil {
		t.Fatal("partial file left behind")
	}
	if data, _ := os.ReadFile(dest); string(data) != "good" {
		t.Fatal("previous backup was clobbered")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".cnf") {
			t.Fatal("option file not cleaned up")
		}
	}
}

// Integration tests run only when a throwaway MariaDB is available, e.g. in
// CI. Set MARIADB_TEST_HOST, MARIADB_TEST_PORT, MARIADB_TEST_DB,
// MARIADB_TEST_USER, MARIADB_TEST_PASSWORD. The user must have only
// SELECT, SHOW VIEW, TRIGGER, EVENT, PROCESS, LOCK TABLES.
func integrationTarget(t *testing.T) Target {
	t.Helper()
	host := os.Getenv("MARIADB_TEST_HOST")
	if host == "" {
		t.Skip("MARIADB_TEST_HOST not set")
	}
	port, _ := strconv.Atoi(os.Getenv("MARIADB_TEST_PORT"))
	if port == 0 {
		port = 3306
	}
	return Target{
		Host:     host,
		Port:     port,
		Database: os.Getenv("MARIADB_TEST_DB"),
		Username: os.Getenv("MARIADB_TEST_USER"),
		Password: os.Getenv("MARIADB_TEST_PASSWORD"),
	}
}

func TestIntegrationTestConnection(t *testing.T) {
	tgt := integrationTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := TestConnection(ctx, tgt)
	if err != nil {
		t.Fatal(err)
	}
	if res.Version == "" || !res.DatabaseVisible {
		t.Fatalf("unexpected result %+v", res)
	}
	tgt.Password = "wrong-" + tgt.Password
	if _, err := TestConnection(ctx, tgt); err == nil {
		t.Fatal("wrong password accepted")
	} else if strings.Contains(err.Error(), tgt.Password) {
		t.Fatal("password leaked in error")
	}
}

func TestIntegrationDump(t *testing.T) {
	tgt := integrationTarget(t)
	bin := os.Getenv("MARIADB_DUMP")
	if bin == "" {
		bin = "mariadb-dump"
	}
	d := &Dumper{Binary: bin, TempDir: t.TempDir(), Timeout: 2 * time.Minute}
	for _, gz := range []bool{false, true} {
		dest := filepath.Join(t.TempDir(), "dump.sql")
		if gz {
			dest += ".gz"
		}
		size, err := d.Dump(context.Background(), tgt, dest, gz)
		if err != nil {
			t.Fatalf("gz=%v: %v", gz, err)
		}
		if size == 0 {
			t.Fatalf("gz=%v: empty dump", gz)
		}
		if _, err := os.Stat(dest + ".partial"); err == nil {
			t.Fatal("partial file still present after success")
		}
	}
}
