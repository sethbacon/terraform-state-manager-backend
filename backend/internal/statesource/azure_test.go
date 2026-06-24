package statesource

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

// fakeAzureBlob imitates enough of the Azure Blob REST API for the connector:
// list-blobs (XML), download, and upload — the registry-frontend pattern of
// pointing the real azblob SDK at an httptest server (no Azurite needed).
func fakeAzureBlob(t *testing.T) (*azureConn, map[string][]byte) {
	t.Helper()
	blobs := map[string][]byte{
		"envs/prod.tfstate": []byte(`{"version":4,"serial":4}`),
		"envs/readme.md":    []byte("not state"),
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/state/") // container name "state"
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "comp=list"):
			prefix := r.URL.Query().Get("prefix")
			var b strings.Builder
			b.WriteString(`<?xml version="1.0" encoding="utf-8"?><EnumerationResults ServiceEndpoint="` +
				srv.URL + `/" ContainerName="state"><Blobs>`)
			for name, data := range blobs {
				if prefix != "" && !strings.HasPrefix(name, prefix) {
					continue
				}
				fmt.Fprintf(&b, `<Blob><Name>%s</Name><Properties><Last-Modified>%s</Last-Modified><Content-Length>%d</Content-Length><BlobType>BlockBlob</BlobType></Properties></Blob>`,
					name, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat), len(data))
			}
			b.WriteString(`</Blobs><NextMarker /></EnumerationResults>`)
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(b.String()))
		case r.Method == http.MethodGet:
			data, ok := blobs[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
			_, _ = w.Write(data)
		case r.Method == http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			blobs[key] = body
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodDelete:
			if _, ok := blobs[key]; !ok {
				w.Header().Set("x-ms-error-code", "BlobNotFound")
				w.WriteHeader(http.StatusNotFound)
				return
			}
			delete(blobs, key)
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := azblob.NewClientWithNoCredential(srv.URL, nil)
	if err != nil {
		t.Fatalf("azblob client: %v", err)
	}
	return &azureConn{client: client, container: "state"}, blobs
}

func TestAzure_NewValidation(t *testing.T) {
	if _, err := newAzure(map[string]any{"container": "c"}, map[string]any{"account_key": "a2V5"}); err == nil {
		t.Error("missing account must error")
	}
	if _, err := newAzure(map[string]any{"account": "acct", "container": "c"}, map[string]any{}); err == nil {
		t.Error("missing account_key must error")
	}
	// A non-base64 key fails shared-key credential construction.
	if _, err := newAzure(map[string]any{"account": "acct", "container": "c"},
		map[string]any{"account_key": "not base64!!"}); err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Errorf("invalid key: %v", err)
	}
	// A valid base64 key constructs without any network call.
	conn, err := newAzure(map[string]any{"account": "acct", "container": "c", "prefix": "envs/"},
		map[string]any{"account_key": "a2V5LWJ5dGVz"})
	if err != nil || conn.prefix != "envs/" {
		t.Fatalf("valid config: %v %+v", err, conn)
	}
}

func TestAzure_ListReadWrite(t *testing.T) {
	conn, blobs := fakeAzureBlob(t)
	ctx := context.Background()

	refs, err := conn.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 || refs[0].Key != "envs/prod.tfstate" {
		t.Fatalf("only .tfstate blobs should list: %+v", refs)
	}
	if refs[0].Size == 0 || refs[0].LastModified == nil {
		t.Errorf("blob properties not mapped: %+v", refs[0])
	}

	// Prefix narrows the listing server-side.
	conn.prefix = "other/"
	if refs, err = conn.List(ctx); err != nil || len(refs) != 0 {
		t.Errorf("prefixed list: %v %+v", err, refs)
	}
	conn.prefix = ""

	rs, err := conn.Read(ctx, "envs/prod.tfstate")
	if err != nil || !strings.Contains(string(rs.Data), `"serial":4`) {
		t.Fatalf("Read: %v", err)
	}
	if _, err := conn.Read(ctx, "missing.tfstate"); err == nil {
		t.Error("missing blob must error")
	}

	if err := conn.Write(ctx, "envs/new.tfstate", []byte(`{"version":4}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if string(blobs["envs/new.tfstate"]) != `{"version":4}` {
		t.Error("Write did not store the blob")
	}
	if err := conn.Delete(ctx, "envs/prod.tfstate"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := blobs["envs/prod.tfstate"]; ok {
		t.Error("Delete did not remove the blob")
	}
	if err := conn.Delete(ctx, "missing.tfstate"); !IsNotFound(err) {
		t.Errorf("missing delete must be IsNotFound, got %v", err)
	}
}
