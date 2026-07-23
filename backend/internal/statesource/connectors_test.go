package statesource

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// --- factory ---

func TestNew_KnownAndUnknownTypes(t *testing.T) {
	if _, err := New("nope", nil, nil); err == nil {
		t.Fatal("expected error for unknown source type")
	}
	// Each new type is wired through the factory (validation errors are fine —
	// the point is the type is recognized, not "unknown source type").
	for _, typ := range []string{"consul", "pg", "kubernetes", "http"} {
		_, err := New(typ, map[string]any{}, nil)
		if err != nil && strings.Contains(err.Error(), "unknown state source type") {
			t.Errorf("type %q is not wired into the factory: %v", typ, err)
		}
	}
}

// --- consul ---

func TestConsulConfigValidation(t *testing.T) {
	if _, err := newConsul(map[string]any{}, nil); err == nil {
		t.Error("expected error for missing address")
	}
	if _, err := newConsul(map[string]any{"address": "x:8500", "scheme": "ftp"}, nil); err == nil {
		t.Error("expected error for bad scheme")
	}
}

func TestConsulListReadWrite(t *testing.T) {
	stateBody := `{"version":4}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Consul-Token"); got != "tok" {
			t.Errorf("missing ACL token header, got %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("recurse") == "true":
			_ = json.NewEncoder(w).Encode([]consulKVEntry{
				{Key: "terraform/prod", Value: base64.StdEncoding.EncodeToString([]byte(stateBody)), ModifyIndex: 41},
				{Key: "terraform/sub/", Value: ""}, // directory placeholder: skipped
			})
		case r.Method == http.MethodGet && r.URL.Query().Get("raw") == "true":
			if r.URL.Path != "/v1/kv/terraform/prod" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(stateBody))
		case r.Method == http.MethodGet: // cas-index fetch before a write
			_ = json.NewEncoder(w).Encode([]consulKVEntry{
				{Key: "terraform/prod", Value: base64.StdEncoding.EncodeToString([]byte(stateBody)), ModifyIndex: 41},
			})
		case r.Method == http.MethodPut:
			// Writes must be check-and-set against the index just fetched.
			if r.URL.Query().Get("cas") != "41" {
				t.Errorf("write must use cas=41, got %q", r.URL.Query().Get("cas"))
			}
			_, _ = w.Write([]byte("true"))
		case r.Method == http.MethodDelete:
			_, _ = w.Write([]byte("true"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	c, err := newConsul(map[string]any{"address": u.Host, "scheme": "http"}, map[string]any{"token": "tok"})
	if err != nil {
		t.Fatalf("newConsul: %v", err)
	}
	refs, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 || refs[0].Key != "terraform/prod" || refs[0].Size != int64(len(stateBody)) {
		t.Fatalf("unexpected refs: %+v", refs)
	}
	// ModifyIndex feeds the sync change marker (same-size edits still detected).
	if refs[0].Version != "41" {
		t.Fatalf("version = %q, want ModifyIndex 41", refs[0].Version)
	}
	rs, err := c.Read(context.Background(), "terraform/prod")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(rs.Data) != stateBody {
		t.Errorf("Read data = %s", rs.Data)
	}
	if err := c.Write(context.Background(), "terraform/prod", []byte(stateBody)); err != nil {
		t.Errorf("Write: %v", err)
	}
	// A missing key reads as ErrNotFound (guarded writes key off this).
	if _, err := c.Read(context.Background(), "terraform/missing"); !IsNotFound(err) {
		t.Errorf("missing key must be IsNotFound, got %v", err)
	}
	if err := c.Delete(context.Background(), "terraform/prod"); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

func TestConsulWriteCASConflict(t *testing.T) {
	// Consul answers the cas-index fetch with index 7 but rejects the PUT
	// ("false" body): a concurrent writer bumped the key. Write must surface a
	// conflict instead of reporting success.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode([]consulKVEntry{{Key: "terraform/prod", ModifyIndex: 7}})
		case http.MethodPut:
			if r.URL.Query().Get("cas") != "7" {
				t.Errorf("cas = %q, want 7", r.URL.Query().Get("cas"))
			}
			_, _ = w.Write([]byte("false"))
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c, err := newConsul(map[string]any{"address": u.Host}, nil)
	if err != nil {
		t.Fatalf("newConsul: %v", err)
	}
	err = c.Write(context.Background(), "terraform/prod", []byte(`{"version":4}`))
	if err == nil || !strings.Contains(err.Error(), "concurrent writer") {
		t.Errorf("CAS rejection must surface a conflict, got %v", err)
	}

	// Fresh keys write with cas=0 (create-only).
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPut:
			if r.URL.Query().Get("cas") != "0" {
				t.Errorf("fresh key cas = %q, want 0", r.URL.Query().Get("cas"))
			}
			_, _ = w.Write([]byte("true"))
		}
	}))
	defer srv2.Close()
	u2, _ := url.Parse(srv2.URL)
	c2, _ := newConsul(map[string]any{"address": u2.Host}, nil)
	if err := c2.Write(context.Background(), "terraform/new", []byte(`{"version":4}`)); err != nil {
		t.Errorf("fresh-key write: %v", err)
	}
}

// The consul connector must implement Locker: the edit pipeline only uses a
// native lock when the interface is satisfied, and only the native lock
// excludes a concurrent `terraform apply` (which knows nothing of TSM's
// app-level PG lock).
var _ Locker = (*consul)(nil)

func TestConsulLockUnlock(t *testing.T) {
	var destroyed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/session/create":
			_, _ = w.Write([]byte(`{"ID":"sess-1"}`))
		case r.Method == http.MethodPut && r.URL.Path == "/v1/kv/terraform/prod/.lock" && r.URL.Query().Get("acquire") == "sess-1":
			_, _ = w.Write([]byte("true"))
		case r.Method == http.MethodPut && r.URL.Path == "/v1/kv/terraform/prod/.lock" && r.URL.Query().Get("release") == "sess-1":
			_, _ = w.Write([]byte("true"))
		case r.Method == http.MethodPut && r.URL.Path == "/v1/session/destroy/sess-1":
			destroyed = true
			_, _ = w.Write([]byte("true"))
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c, err := newConsul(map[string]any{"address": u.Host}, nil)
	if err != nil {
		t.Fatalf("newConsul: %v", err)
	}

	id, err := c.Lock(context.Background(), "terraform/prod")
	if err != nil || id != "sess-1" {
		t.Fatalf("Lock: %v (id %q)", err, id)
	}
	if err := c.Unlock(context.Background(), "terraform/prod", id); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if !destroyed {
		t.Error("Unlock must destroy the lock's session")
	}
}

func TestConsulLockConflict(t *testing.T) {
	// The acquire is rejected ("false"): terraform (or another edit) holds
	// <key>/.lock. Lock must error and clean its session up.
	var destroyed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/session/create":
			_, _ = w.Write([]byte(`{"ID":"sess-2"}`))
		case strings.HasSuffix(r.URL.Path, "/.lock"):
			_, _ = w.Write([]byte("false"))
		case r.URL.Path == "/v1/session/destroy/sess-2":
			destroyed = true
			_, _ = w.Write([]byte("true"))
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c, _ := newConsul(map[string]any{"address": u.Host}, nil)

	_, err := c.Lock(context.Background(), "terraform/prod")
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Errorf("rejected acquire must surface a lock conflict, got %v", err)
	}
	if !destroyed {
		t.Error("a failed acquire must destroy the orphaned session")
	}
}

func TestConsulListSkipsLockArtifacts(t *testing.T) {
	stateBody := `{"version":4}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]consulKVEntry{
			{Key: "terraform/prod", Value: base64.StdEncoding.EncodeToString([]byte(stateBody)), ModifyIndex: 41},
			{Key: "terraform/prod/.lock", Value: ""},     // TSM/terraform lock key
			{Key: "terraform/prod/.lockinfo", Value: ""}, // terraform lock metadata
		})
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c, _ := newConsul(map[string]any{"address": u.Host}, nil)

	refs, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 || refs[0].Key != "terraform/prod" {
		t.Errorf("lock artifacts must not list as states: %+v", refs)
	}
}

func TestConsulDeleteError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c, err := newConsul(map[string]any{"address": u.Host}, nil)
	if err != nil {
		t.Fatalf("newConsul: %v", err)
	}
	if err := c.Delete(context.Background(), "terraform/x"); err == nil {
		t.Error("non-200 delete must surface an error")
	}
}

// --- pg ---

func TestPGConfigValidation(t *testing.T) {
	if _, err := newPG(map[string]any{}, map[string]any{}); err == nil {
		t.Error("expected error for missing conn_str")
	}
	// SQL-injection hardening: schema_name must be a plain identifier.
	for _, bad := range []string{`x"; DROP TABLE states; --`, "a.b", "a b", "1abc", `a"b`} {
		if _, err := newPG(map[string]any{"schema_name": bad}, map[string]any{"conn_str": "postgres://x"}); err == nil {
			t.Errorf("schema_name %q should be rejected", bad)
		}
	}
	if _, err := newPG(map[string]any{"schema_name": "terraform_remote_state"}, map[string]any{"conn_str": "postgres://x"}); err != nil {
		t.Errorf("valid schema rejected: %v", err)
	}
	// config.conn_str is unencrypted and echoed by the API: a password-bearing
	// DSN there (URL or libpq keyword form) must be rejected, passwordless allowed.
	for _, bad := range []string{
		"postgres://user:hunter2@db.example.com/states",
		"host=db.example.com user=u password=hunter2",
		"host=db.example.com PASSWORD = hunter2",
	} {
		if _, err := newPG(map[string]any{"conn_str": bad}, map[string]any{}); err == nil ||
			!strings.Contains(err.Error(), "credentials.conn_str") {
			t.Errorf("password-bearing config.conn_str %q should be rejected, got %v", bad, err)
		}
	}
	if _, err := newPG(map[string]any{"conn_str": "postgres://user@db.example.com/states?sslmode=disable"}, map[string]any{}); err != nil {
		t.Errorf("passwordless config.conn_str rejected: %v", err)
	}
	// credentials.conn_str is the sanctioned home for a password-bearing DSN.
	if _, err := newPG(map[string]any{}, map[string]any{"conn_str": "postgres://user:hunter2@db.example.com/states"}); err != nil {
		t.Errorf("password in credentials.conn_str rejected: %v", err)
	}
}

// --- kubernetes ---

func TestK8sConfigValidation(t *testing.T) {
	// Explicit server config requires no kubeconfig on disk.
	if _, err := newK8s(map[string]any{"server": "://bad"}, map[string]any{"token": "t"}); err == nil {
		t.Error("expected error for invalid server URL")
	}
	if _, err := newK8s(map[string]any{"server": "https://k8s.example.com", "ca_cert": "not-pem"}, map[string]any{"token": "t"}); err == nil {
		t.Error("expected error for invalid ca_cert PEM")
	}
	if _, err := newK8s(map[string]any{"server": "https://k8s.example.com"}, map[string]any{"token": "t"}); err != nil {
		t.Errorf("valid explicit config rejected: %v", err)
	}
}

func TestK8sListAndRead(t *testing.T) {
	stateBody := `{"version":4}`
	encoded := base64.StdEncoding.EncodeToString([]byte(stateBody))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("missing bearer token, got %q", got)
		}
		switch r.URL.Path {
		case "/api/v1/namespaces/tf/secrets":
			_, _ = w.Write([]byte(`{"items":[
				{"metadata":{"name":"tfstate-default-app","namespace":"tf","creationTimestamp":"2026-01-02T03:04:05Z"},"data":{"tfstate":"` + encoded + `"}},
				{"metadata":{"name":"unrelated","namespace":"tf"},"data":{"other":"eA=="}}
			]}`))
		case "/api/v1/namespaces/tf/secrets/tfstate-default-app":
			_, _ = w.Write([]byte(`{"metadata":{"name":"tfstate-default-app","namespace":"tf"},"data":{"tfstate":"` + encoded + `"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, err := newK8s(map[string]any{"server": srv.URL, "namespace": "tf"}, map[string]any{"token": "tok"})
	if err != nil {
		t.Fatalf("newK8s: %v", err)
	}
	refs, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 || refs[0].Key != "tf/tfstate-default-app" {
		t.Fatalf("unexpected refs: %+v", refs)
	}
	rs, err := c.Read(context.Background(), "tf/tfstate-default-app")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(rs.Data) != stateBody {
		t.Errorf("Read data = %s", rs.Data)
	}
	if _, err := c.Read(context.Background(), "no-slash"); err == nil {
		t.Error("expected error for malformed key")
	}
	if err := c.Delete(context.Background(), "tf/tfstate-default-app"); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if err := c.Delete(context.Background(), "tf/gone"); !IsNotFound(err) {
		t.Errorf("missing delete must be IsNotFound, got %v", err)
	}
}

// --- http backend ---

func TestHTTPBackendConfigValidation(t *testing.T) {
	if _, err := newHTTPBackend(map[string]any{}, nil); err == nil {
		t.Error("expected error for missing address")
	}
	if _, err := newHTTPBackend(map[string]any{"address": "ftp://x"}, nil); err == nil {
		t.Error("expected error for non-http address")
	}
	if _, err := newHTTPBackend(map[string]any{"address": "https://x.io/state", "lock_address": "://bad"}, nil); err == nil {
		t.Error("expected error for invalid lock_address")
	}
}

// The Locker interface must be exposed ONLY when a lock_address is configured —
// otherwise the edit pipeline would treat the unsupported lock as "already
// locked" instead of falling back to the app-level DB lock.
func TestHTTPBackendLockerGating(t *testing.T) {
	plain, err := newHTTPBackend(map[string]any{"address": "https://x.io/state"}, nil)
	if err != nil {
		t.Fatalf("newHTTPBackend: %v", err)
	}
	if _, isLocker := plain.(Locker); isLocker {
		t.Error("connector without lock_address must not implement Locker")
	}
	locking, err := newHTTPBackend(map[string]any{"address": "https://x.io/state", "lock_address": "https://x.io/lock"}, nil)
	if err != nil {
		t.Fatalf("newHTTPBackend: %v", err)
	}
	if _, isLocker := locking.(Locker); !isLocker {
		t.Error("connector with lock_address must implement Locker")
	}
}

func TestHTTPBackendReadWriteLock(t *testing.T) {
	stateBody := `{"version":4}`
	var wroteBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		if user != "u" || pass != "p" {
			t.Errorf("missing basic auth: %q/%q", user, pass)
		}
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/state":
			w.Header().Set("Last-Modified", "Tue, 09 Jun 2026 01:02:03 GMT")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/state":
			_, _ = w.Write([]byte(stateBody))
		case r.Method == http.MethodPost && r.URL.Path == "/state":
			body, _ := io.ReadAll(r.Body)
			wroteBody = string(body)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && r.URL.Path == "/state":
			w.WriteHeader(http.StatusOK)
		case r.Method == "LOCK" && r.URL.Path == "/lock":
			w.WriteHeader(http.StatusOK)
		case r.Method == "UNLOCK" && r.URL.Path == "/lock":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	conn, err := newHTTPBackend(
		map[string]any{"address": srv.URL + "/state", "lock_address": srv.URL + "/lock"},
		map[string]any{"username": "u", "password": "p"})
	if err != nil {
		t.Fatalf("newHTTPBackend: %v", err)
	}
	refs, err := conn.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 || refs[0].Key != httpStateKey || refs[0].LastModified == nil {
		t.Fatalf("unexpected refs: %+v", refs)
	}
	// HEAD here sends no Content-Length (client sees -1): the ref must clamp
	// to 0 so the API layer's analysis-store size overlay applies.
	if refs[0].Size != 0 {
		t.Fatalf("size = %d, want 0 when Content-Length is absent", refs[0].Size)
	}
	rs, err := conn.Read(context.Background(), httpStateKey)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(rs.Data) != stateBody {
		t.Errorf("Read data = %s", rs.Data)
	}
	if err := conn.Write(context.Background(), httpStateKey, []byte(stateBody)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if wroteBody != stateBody {
		t.Errorf("written body = %s", wroteBody)
	}
	locker := conn.(Locker)
	id, err := locker.Lock(context.Background(), httpStateKey)
	if err != nil || id == "" {
		t.Fatalf("Lock: id=%q err=%v", id, err)
	}
	if err := locker.Unlock(context.Background(), httpStateKey, id); err != nil {
		t.Errorf("Unlock: %v", err)
	}
	if err := conn.Delete(context.Background(), httpStateKey); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

func TestHTTPBackendDeleteNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	conn, err := newHTTPBackend(map[string]any{"address": srv.URL + "/state"}, nil)
	if err != nil {
		t.Fatalf("newHTTPBackend: %v", err)
	}
	if err := conn.Delete(context.Background(), httpStateKey); !IsNotFound(err) {
		t.Errorf("404 delete must be IsNotFound, got %v", err)
	}
}
