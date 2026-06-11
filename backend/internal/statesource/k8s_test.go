package statesource

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeK8s serves the secrets API surface the connector uses, backed by a map of
// secret name → data keys.
func fakeK8s(t *testing.T) (*k8s, map[string]map[string]string) {
	t.Helper()
	secrets := map[string]map[string]string{
		"tfstate-default-app": {"tfstate": base64.StdEncoding.EncodeToString([]byte(`{"version":4,"serial":5}`))},
		"unrelated":           {"other": base64.StdEncoding.EncodeToString([]byte("x"))},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k8s-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/api/v1/namespaces/default/secrets" && r.Method == http.MethodGet:
			items := make([]map[string]any, 0, len(secrets))
			for name, data := range secrets {
				items = append(items, map[string]any{
					"metadata": map[string]any{"name": name, "namespace": "default", "creationTimestamp": "2026-06-01T00:00:00Z"},
					"data":     data,
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		case strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/default/secrets/"):
			name := strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/default/secrets/")
			data, ok := secrets[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			switch r.Method {
			case http.MethodGet:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"metadata": map[string]any{"name": name, "namespace": "default"},
					"data":     data,
				})
			case http.MethodPatch:
				var patch struct {
					Data map[string]string `json:"data"`
				}
				_ = json.NewDecoder(r.Body).Decode(&patch)
				for k, v := range patch.Data {
					data[k] = v
				}
				fmt.Fprint(w, `{}`)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	conn, err := newK8s(
		map[string]any{"server": srv.URL, "namespace": "default"},
		map[string]any{"token": "k8s-token"},
	)
	if err != nil {
		t.Fatalf("newK8s: %v", err)
	}
	return conn, secrets
}

func TestK8s_NewValidation(t *testing.T) {
	// No server and no kubeconfig anywhere reachable.
	t.Setenv("KUBECONFIG", "/nonexistent/kubeconfig")
	t.Setenv("HOME", t.TempDir())
	if _, err := newK8s(map[string]any{}, map[string]any{}); err == nil {
		t.Error("no server/kubeconfig must error")
	}
	if _, err := newK8s(map[string]any{"server": "not a url"}, map[string]any{"token": "t"}); err == nil {
		t.Error("invalid server URL must error")
	}
	if _, err := newK8s(map[string]any{"server": "https://k8s.example", "ca_cert": "not pem"}, map[string]any{"token": "t"}); err == nil {
		t.Error("garbage CA PEM must error")
	}
}

func TestK8s_ListOnlyTfstateSecrets(t *testing.T) {
	conn, _ := fakeK8s(t)
	refs, err := conn.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 || refs[0].Key != "default/tfstate-default-app" {
		t.Fatalf("only secrets with a tfstate key should list: %+v", refs)
	}
	if refs[0].Size == 0 || refs[0].LastModified == nil {
		t.Errorf("decoded size/timestamp not mapped: %+v", refs[0])
	}
}

func TestK8s_ReadWrite(t *testing.T) {
	conn, secrets := fakeK8s(t)
	ctx := context.Background()

	rs, err := conn.Read(ctx, "default/tfstate-default-app")
	if err != nil || !strings.Contains(string(rs.Data), `"serial":5`) {
		t.Fatalf("Read: %v", err)
	}

	// Key validation happens before any network call.
	if _, err := conn.Read(ctx, "no-namespace"); err == nil {
		t.Error("malformed key must error")
	}
	if _, err := conn.Read(ctx, "default/missing"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing secret: %v", err)
	}
	if _, err := conn.Read(ctx, "default/unrelated"); err == nil || !strings.Contains(err.Error(), "no tfstate key") {
		t.Errorf("secret without tfstate: %v", err)
	}

	// Write patches only the tfstate key (strategic merge).
	if err := conn.Write(ctx, "default/tfstate-default-app", []byte(`{"version":4,"serial":6}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	stored, _ := base64.StdEncoding.DecodeString(secrets["tfstate-default-app"]["tfstate"])
	if !strings.Contains(string(stored), `"serial":6`) {
		t.Errorf("Write did not patch the secret: %s", stored)
	}
	if err := conn.Write(ctx, "default/missing", []byte("{}")); err == nil {
		t.Error("write to a missing secret must error")
	}
}

func TestK8s_AuthFailureSurfaces(t *testing.T) {
	conn, _ := fakeK8s(t)
	conn.token = "wrong"
	if _, err := conn.List(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("auth failure must surface the status: %v", err)
	}
}
