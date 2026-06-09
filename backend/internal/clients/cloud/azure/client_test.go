package azure

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const (
	vnetID    = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-platform/providers/Microsoft.Network/virtualNetworks/vnet-main"
	storageID = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-platform/providers/Microsoft.Storage/storageAccounts/sttfstateprod"
	goneVMID  = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-platform/providers/Microsoft.Compute/virtualMachines/vm-gone"
	testToken = "fake-arm-token"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

// staticCred is a Credential that always returns the same token, or an error.
type staticCred struct {
	token string
	err   error
}

func (c staticCred) Token(context.Context) (string, error) { return c.token, c.err }

// newMockARM returns an httptest server that serves the recorded ARM fixtures by
// resource path and records the last Authorization header and api-version seen.
func newMockARM(t *testing.T) (*httptest.Server, *string, *string) {
	t.Helper()
	var seenAuth, seenVersion string
	mux := http.NewServeMux()

	serve := func(fixture string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			seenAuth = r.Header.Get("Authorization")
			seenVersion = r.URL.Query().Get("api-version")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(loadFixture(t, fixture))
		}
	}

	mux.HandleFunc(vnetID, serve("virtual_network.json"))
	mux.HandleFunc(storageID, serve("storage_account.json"))
	mux.HandleFunc(goneVMID, func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(loadFixture(t, "not_found.json"))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &seenAuth, &seenVersion
}

func newLiveReader(t *testing.T, baseURL string, cred Credential) ResourceReader {
	t.Helper()
	r, err := NewARMReader(Config{Credential: cred, BaseURL: baseURL})
	if err != nil {
		t.Fatalf("NewARMReader: %v", err)
	}
	return r
}

func TestNewARMReader_RequiresCredential(t *testing.T) {
	if _, err := NewARMReader(Config{}); err == nil {
		t.Error("expected error when Credential is nil")
	}
}

func TestReadResource_Present(t *testing.T) {
	srv, seenAuth, seenVersion := newMockARM(t)
	r := newLiveReader(t, srv.URL, staticCred{token: testToken})

	state, err := r.ReadResource(context.Background(), vnetID)
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if state.Existence != ExistencePresent {
		t.Fatalf("Existence = %q, want present", state.Existence)
	}
	if state.Properties[PropLocation] != "eastus" {
		t.Errorf("location = %q, want eastus", state.Properties[PropLocation])
	}
	if state.Properties[PropProvisioningState] != "Succeeded" {
		t.Errorf("provisioning_state = %q, want Succeeded", state.Properties[PropProvisioningState])
	}
	if *seenAuth != "Bearer "+testToken {
		t.Errorf("Authorization = %q, want Bearer %s", *seenAuth, testToken)
	}
	if *seenVersion != "2023-09-01" {
		t.Errorf("api-version = %q, want 2023-09-01", *seenVersion)
	}
}

func TestReadResource_PresentWithSKU(t *testing.T) {
	srv, _, _ := newMockARM(t)
	r := newLiveReader(t, srv.URL, staticCred{token: testToken})

	state, err := r.ReadResource(context.Background(), storageID)
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if state.Existence != ExistencePresent {
		t.Fatalf("Existence = %q, want present", state.Existence)
	}
	if state.Properties[PropSKU] != "Standard_LRS" {
		t.Errorf("sku = %q, want Standard_LRS", state.Properties[PropSKU])
	}
	if state.Properties[PropKind] != "StorageV2" {
		t.Errorf("kind = %q, want StorageV2", state.Properties[PropKind])
	}
}

func TestReadResource_Missing(t *testing.T) {
	srv, _, _ := newMockARM(t)
	r := newLiveReader(t, srv.URL, staticCred{token: testToken})

	state, err := r.ReadResource(context.Background(), goneVMID)
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if state.Existence != ExistenceMissing {
		t.Errorf("Existence = %q, want missing", state.Existence)
	}
}

func TestReadResource_Forbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	r := newLiveReader(t, srv.URL, staticCred{token: testToken})

	state, err := r.ReadResource(context.Background(), vnetID)
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if state.Existence != ExistenceUnknown {
		t.Errorf("Existence = %q, want unknown", state.Existence)
	}
	if state.Note != "access denied" {
		t.Errorf("Note = %q, want access denied", state.Note)
	}
}

func TestReadResource_CredentialUnavailable(t *testing.T) {
	srv, _, _ := newMockARM(t)
	r := newLiveReader(t, srv.URL, staticCred{err: ErrCredentialUnavailable})

	state, err := r.ReadResource(context.Background(), vnetID)
	if err != nil {
		t.Fatalf("ReadResource should not error on missing credential: %v", err)
	}
	if state.Existence != ExistenceUnknown {
		t.Errorf("Existence = %q, want unknown", state.Existence)
	}
	if state.Note != "credential unavailable" {
		t.Errorf("Note = %q, want credential unavailable", state.Note)
	}
}

func TestReadResource_UnparseableID(t *testing.T) {
	srv, _, _ := newMockARM(t)
	r := newLiveReader(t, srv.URL, staticCred{token: testToken})

	state, err := r.ReadResource(context.Background(), "not-an-arm-id")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if state.Existence != ExistenceUnknown {
		t.Errorf("Existence = %q, want unknown", state.Existence)
	}
	if state.Note != "unparseable id" {
		t.Errorf("Note = %q, want unparseable id", state.Note)
	}
}

func TestReadResource_UnexpectedStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad"}`))
	}))
	t.Cleanup(srv.Close)
	r := newLiveReader(t, srv.URL, staticCred{token: testToken})

	if _, err := r.ReadResource(context.Background(), vnetID); err == nil {
		t.Error("expected error for unexpected 400 status")
	}
}

func TestCredentialFunc(t *testing.T) {
	var cred Credential = CredentialFunc(func(context.Context) (string, error) {
		return "tok", nil
	})
	got, err := cred.Token(context.Background())
	if err != nil || got != "tok" {
		t.Errorf("CredentialFunc.Token() = %q, %v; want tok, nil", got, err)
	}
}
