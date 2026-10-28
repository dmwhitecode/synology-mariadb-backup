package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmwhitecode/synology-mariadb-backup/internal/dumper"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/model"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/scheduler"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/store"
	"github.com/dmwhitecode/synology-mariadb-backup/internal/vault"
)

type env struct {
	ts     *httptest.Server
	client *http.Client
	store  *store.Store
	vault  *vault.Vault
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.Open(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &dumper.Dumper{Binary: "mariadb-dump-not-present", TempDir: dir}
	sc := scheduler.New(st, v, d, dir, log)
	if err := sc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv, err := New(st, v, sc, dir, dir, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(func() { ts.Close(); st.Close() })
	return &env{ts: ts, client: client, store: st, vault: v}
}

func (e *env) get(t *testing.T, path string) *http.Response {
	t.Helper()
	res, err := e.client.Get(e.ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (e *env) post(t *testing.T, path string, form url.Values) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", e.ts.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.ts.URL)
	res, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (e *env) postJSON(t *testing.T, path, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", e.ts.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", e.ts.URL)
	res, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res, string(b)
}

func (e *env) setup(t *testing.T) {
	t.Helper()
	res := e.post(t, "/setup", url.Values{"username": {"admin"}, "password": {"correct-horse-battery"}, "confirm": {"correct-horse-battery"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup status %d", res.StatusCode)
	}
}

func body(t *testing.T, res *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return string(b)
}

func TestFirstRunRedirectsToSetup(t *testing.T) {
	e := newEnv(t)
	res := e.get(t, "/")
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/setup" {
		t.Fatalf("got %d -> %s", res.StatusCode, res.Header.Get("Location"))
	}
	res = e.get(t, "/login")
	if res.Header.Get("Location") != "/setup" {
		t.Fatal("login should redirect to setup before an admin exists")
	}
}

func TestSetupLoginLogout(t *testing.T) {
	e := newEnv(t)
	// Weak password rejected.
	res := e.post(t, "/setup", url.Values{"username": {"admin"}, "password": {"short"}, "confirm": {"short"}})
	if res.StatusCode != http.StatusOK || !strings.Contains(body(t, res), "at least 10") {
		t.Fatal("weak password accepted")
	}
	e.setup(t)

	// Setup cannot be repeated.
	res = e.post(t, "/setup", url.Values{"username": {"x"}, "password": {"correct-horse-battery"}, "confirm": {"correct-horse-battery"}})
	if res.Header.Get("Location") != "/login" {
		t.Fatal("second setup should redirect to login")
	}

	// Logged in after setup.
	res = e.get(t, "/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected connections page, got %d", res.StatusCode)
	}
	res = e.post(t, "/logout", nil)
	res = e.get(t, "/")
	if res.Header.Get("Location") != "/login" {
		t.Fatal("still logged in after logout")
	}

	res = e.post(t, "/login", url.Values{"username": {"admin"}, "password": {"wrong-password"}})
	if res.StatusCode != http.StatusOK || !strings.Contains(body(t, res), "Incorrect") {
		t.Fatal("wrong password accepted")
	}
	res = e.post(t, "/login", url.Values{"username": {"admin"}, "password": {"correct-horse-battery"}})
	if res.Header.Get("Location") != "/" {
		t.Fatal("login failed")
	}
}

func TestLoginLockout(t *testing.T) {
	e := newEnv(t)
	e.setup(t)
	e.post(t, "/logout", nil)
	for i := 0; i < maxFailures; i++ {
		e.post(t, "/login", url.Values{"username": {"admin"}, "password": {"nope"}})
	}
	res := e.post(t, "/login", url.Values{"username": {"admin"}, "password": {"correct-horse-battery"}})
	if res.StatusCode != http.StatusOK || !strings.Contains(body(t, res), "Too many") {
		t.Fatal("expected lockout after repeated failures")
	}
}

func TestCrossSitePostRejected(t *testing.T) {
	e := newEnv(t)
	e.setup(t)
	req, _ := http.NewRequest("POST", e.ts.URL+"/logout", nil)
	req.Header.Set("Origin", "https://evil.example")
	res, _ := e.client.Do(req)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST got %d", res.StatusCode)
	}
	req, _ = http.NewRequest("POST", e.ts.URL+"/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	res, _ = e.client.Do(req)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site POST got %d", res.StatusCode)
	}
}

func TestConnectionLifecycle(t *testing.T) {
	e := newEnv(t)
	e.setup(t)

	// Form defaults to port 3306.
	res := e.get(t, "/connections/new")
	if !strings.Contains(body(t, res), `name="port" value="3306"`) {
		t.Fatal("port not pre-filled with 3306")
	}

	// Validation errors re-render the form.
	res = e.post(t, "/connections/new", url.Values{"name": {"X"}})
	if res.StatusCode != http.StatusOK || !strings.Contains(body(t, res), "Host is required") {
		t.Fatal("validation did not fire")
	}

	form := url.Values{
		"name": {"Prod Shop"}, "host": {"db.local"}, "port": {""}, "database": {"shop"},
		"username": {"ro"}, "password": {"s3cret-pw"}, "enabled": {"on"},
		"schedule_inherit": {"on"}, "retention_inherit": {"on"}, "naming_inherit": {"on"},
	}
	res = e.post(t, "/connections/new", form)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("create status %d: %s", res.StatusCode, body(t, res))
	}
	conns, _ := e.store.ListConnections(context.Background())
	if len(conns) != 1 || conns[0].Port != 3306 || conns[0].Slug != "prod-shop" {
		t.Fatalf("unexpected %+v", conns)
	}
	c := conns[0]
	if c.PasswordEnc == "" || strings.Contains(c.PasswordEnc, "s3cret-pw") {
		t.Fatal("password stored in clear or missing")
	}
	if pw, _ := e.vault.Decrypt(c.PasswordEnc); pw != "s3cret-pw" {
		t.Fatal("password did not round-trip through vault")
	}

	// The password never comes back in any page.
	for _, p := range []string{"/", "/connections/1/edit", "/connections/1/runs"} {
		if strings.Contains(body(t, e.get(t, p)), "s3cret-pw") {
			t.Fatalf("password leaked on %s", p)
		}
	}

	// Edit with empty password keeps it; override retention.
	form.Set("password", "")
	form.Set("name", "Renamed")
	form.Del("retention_inherit")
	form.Set("retention_keep", "3")
	res = e.post(t, "/connections/1/edit", form)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("edit status %d: %s", res.StatusCode, body(t, res))
	}
	c, _ = e.store.GetConnection(context.Background(), 1)
	if pw, _ := e.vault.Decrypt(c.PasswordEnc); pw != "s3cret-pw" || c.Name != "Renamed" || c.RetentionKeep == nil || *c.RetentionKeep != 3 {
		t.Fatalf("edit wrong: %+v", c)
	}

	// Run now with a missing dump binary records a failure, not a crash.
	res = e.post(t, "/connections/1/run", nil)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("run status %d", res.StatusCode)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		runs, _ := e.store.ListRuns(context.Background(), 1, 1)
		if len(runs) == 1 && runs[0].Status == model.StatusFailed {
			if runs[0].Error == "" || strings.Contains(runs[0].Error, "s3cret-pw") {
				t.Fatalf("bad run error %q", runs[0].Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not finish: %+v", runs)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Deleting is refused only while a run is in flight; it has finished now.

	res = e.post(t, "/connections/1/toggle", nil)
	c, _ = e.store.GetConnection(context.Background(), 1)
	if c.Enabled {
		t.Fatal("toggle did not disable")
	}

	res = e.post(t, "/connections/1/delete", nil)
	if _, err := e.store.GetConnection(context.Background(), 1); err == nil {
		t.Fatal("not deleted")
	}
	if res := e.get(t, "/connections/1/edit"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted connection edit got %d", res.StatusCode)
	}
}

func TestSettingsSave(t *testing.T) {
	e := newEnv(t)
	e.setup(t)
	res := e.post(t, "/settings", url.Values{
		"schedule_mode": {"weekly"}, "schedule_time": {"03:15"}, "schedule_days": {"1", "4"},
		"retention_keep": {"14"},
		"naming_use_db_name": {"on"}, "naming_ts_prefix": {"on"}, "naming_ts_format": {"YYYYMMDD"}, "naming_gzip": {"on"},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("status %d: %s", res.StatusCode, body(t, res))
	}
	d, _ := e.store.GetDefaults(context.Background())
	if d.RetentionKeep != 14 || d.Schedule.Describe() != "Mon, Thu at 03:15" || !d.Naming.Gzip || !d.Naming.TimestampPrefix {
		t.Fatalf("defaults wrong: %+v", d)
	}

	// Invalid input re-renders with errors and does not save.
	res = e.post(t, "/settings", url.Values{"schedule_mode": {"cron"}, "schedule_cron": {"bad"}, "retention_keep": {"0"}, "naming_use_db_name": {"on"}})
	b := body(t, res)
	if res.StatusCode != http.StatusOK || !strings.Contains(b, "Schedule:") || !strings.Contains(b, "Retention:") || !strings.Contains(b, "Filename:") {
		t.Fatalf("expected three errors, got: %d", res.StatusCode)
	}
	d, _ = e.store.GetDefaults(context.Background())
	if d.RetentionKeep != 14 {
		t.Fatal("invalid settings were saved")
	}
}

func TestAPIs(t *testing.T) {
	e := newEnv(t)
	// Unauthenticated API call gets JSON 401, not a redirect.
	e.setup(t)
	e.post(t, "/logout", nil)
	res, b := e.postJSON(t, "/api/naming-preview", `{"database":"x","naming":{"use_db_name":true}}`)
	if res.StatusCode != http.StatusUnauthorized || !strings.Contains(b, "not logged in") {
		t.Fatalf("got %d %s", res.StatusCode, b)
	}
	e.post(t, "/login", url.Values{"username": {"admin"}, "password": {"correct-horse-battery"}})

	_, b = e.postJSON(t, "/api/naming-preview", `{"database":"shop","naming":{"use_db_name":true,"prefix":"p-","timestamp_suffix":true,"timestamp_format":"YYYY","gzip":true}}`)
	if !strings.Contains(b, `"filename":"p-shop_2026.sql.gz"`) {
		t.Fatalf("preview: %s", b)
	}
	_, b = e.postJSON(t, "/api/naming-preview", `{"database":"shop","naming":{"use_db_name":true}}`)
	if !strings.Contains(b, `"ok":false`) {
		t.Fatalf("preview should fail without timestamp: %s", b)
	}

	// Test connection: no password and no id -> clear error, no probe.
	_, b = e.postJSON(t, "/api/test-connection", `{"host":"127.0.0.1","port":1,"database":"d","username":"u","password":""}`)
	if !strings.Contains(b, "enter a password") {
		t.Fatalf("got %s", b)
	}
	// Unreachable host returns ok=false with a scrubbed error and never panics.
	_, b = e.postJSON(t, "/api/test-connection", `{"host":"127.0.0.1","port":1,"database":"d","username":"u","password":"pw-leak-check"}`)
	if !strings.Contains(b, `"ok":false`) || strings.Contains(b, "pw-leak-check") {
		t.Fatalf("got %s", b)
	}
	// Unknown fields rejected.
	res, _ = e.postJSON(t, "/api/test-connection", `{"host":"h","evil":1}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field accepted: %d", res.StatusCode)
	}
}

// TestPagesRenderFully guards against template execution errors, which are
// logged rather than returned: a page that stops mid-render has no </html>.
func TestPagesRenderFully(t *testing.T) {
	e := newEnv(t)
	e.setup(t)
	e.post(t, "/connections/new", url.Values{
		"name": {"Prod"}, "host": {"db"}, "database": {"shop"}, "username": {"ro"}, "password": {"pw-pw-pw-pw"},
		"enabled": {"on"}, "schedule_mode": {"weekly"}, "schedule_time": {"01:00"}, "schedule_days": {"2"},
		"retention_keep": {"5"}, "naming_use_db_name": {"on"}, "naming_ts_suffix": {"on"},
	})
	e.post(t, "/connections/1/run", nil)
	time.Sleep(300 * time.Millisecond)
	for _, p := range []string{"/", "/connections/new", "/connections/1/edit", "/connections/1/runs", "/settings"} {
		res := e.get(t, p)
		b := body(t, res)
		if res.StatusCode != http.StatusOK || !strings.Contains(b, "</html>") {
			t.Errorf("%s: status %d, rendered %d bytes, complete=%v", p, res.StatusCode, len(b), strings.Contains(b, "</html>"))
		}
	}
	e.post(t, "/logout", nil)
	for _, p := range []string{"/login"} {
		if b := body(t, e.get(t, p)); !strings.Contains(b, "</html>") {
			t.Errorf("%s did not render fully", p)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	res := e.get(t, "/healthz")
	if res.Header.Get("Content-Security-Policy") == "" || res.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatal("security headers missing")
	}
}
