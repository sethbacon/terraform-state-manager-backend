package statesource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// mockGCS implements gcsAPI in memory (registry storage-backend pattern).
type mockGCS struct {
	objects   map[string][]byte
	readerErr error
	writeErr  error
	closeErr  error
	listErr   error
	written   map[string]string // object → content type
}

type mockIterator struct {
	attrs []*storage.ObjectAttrs
	err   error
	i     int
}

func (m *mockIterator) Next() (*storage.ObjectAttrs, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.i >= len(m.attrs) {
		return nil, iterator.Done
	}
	a := m.attrs[m.i]
	m.i++
	return a, nil
}

func (m *mockGCS) Objects(_ context.Context, _ string, q *storage.Query) gcsObjectIterator {
	if m.listErr != nil {
		return &mockIterator{err: m.listErr}
	}
	var attrs []*storage.ObjectAttrs
	for name, data := range m.objects {
		if q.Prefix != "" && !strings.HasPrefix(name, q.Prefix) {
			continue
		}
		attrs = append(attrs, &storage.ObjectAttrs{Name: name, Size: int64(len(data)), Updated: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)})
	}
	return &mockIterator{attrs: attrs}
}

func (m *mockGCS) NewReader(_ context.Context, _, object string) (io.ReadCloser, error) {
	if m.readerErr != nil {
		return nil, m.readerErr
	}
	data, ok := m.objects[object]
	if !ok {
		return nil, fmt.Errorf("object %q does not exist", object)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

type mockWriter struct {
	m      *mockGCS
	object string
	buf    bytes.Buffer
}

func (w *mockWriter) Write(p []byte) (int, error) {
	if w.m.writeErr != nil {
		return 0, w.m.writeErr
	}
	return w.buf.Write(p)
}

func (w *mockWriter) Close() error {
	if w.m.closeErr != nil {
		return w.m.closeErr
	}
	w.m.objects[w.object] = w.buf.Bytes()
	return nil
}

func (m *mockGCS) NewWriter(_ context.Context, _, object, contentType string) io.WriteCloser {
	if m.written == nil {
		m.written = map[string]string{}
	}
	m.written[object] = contentType
	return &mockWriter{m: m, object: object}
}

func newMockGCSConn(prefix string) (*gcsConn, *mockGCS) {
	m := &mockGCS{objects: map[string][]byte{
		"envs/prod.tfstate": []byte(`{"version":4,"serial":3}`),
		"envs/notes.txt":    []byte("skip"),
	}}
	return &gcsConn{client: m, bucket: "state-bucket", prefix: prefix}, m
}

func TestGCS_List(t *testing.T) {
	conn, m := newMockGCSConn("")
	refs, err := conn.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 || refs[0].Key != "envs/prod.tfstate" {
		t.Fatalf("only .tfstate objects should list: %+v", refs)
	}
	if refs[0].Size == 0 || refs[0].LastModified == nil {
		t.Errorf("attrs not mapped: %+v", refs[0])
	}

	// Prefix is passed through to the query.
	conn2, _ := newMockGCSConn("other/")
	if refs, err := conn2.List(context.Background()); err != nil || len(refs) != 0 {
		t.Errorf("prefixed list: %v %+v", err, refs)
	}

	m.listErr = errors.New("perm denied")
	if _, err := conn.List(context.Background()); err == nil {
		t.Error("iterator errors must surface")
	}
}

func TestGCS_Read(t *testing.T) {
	conn, m := newMockGCSConn("")
	rs, err := conn.Read(context.Background(), "envs/prod.tfstate")
	if err != nil || !strings.Contains(string(rs.Data), `"serial":3`) {
		t.Fatalf("Read: %v", err)
	}
	if _, err := conn.Read(context.Background(), "missing.tfstate"); err == nil {
		t.Error("missing object must error")
	}
	m.readerErr = errors.New("403")
	if _, err := conn.Read(context.Background(), "envs/prod.tfstate"); err == nil || !strings.Contains(err.Error(), "gs://state-bucket") {
		t.Errorf("reader errors must surface with the gs:// path: %v", err)
	}
}

func TestGCS_Write(t *testing.T) {
	conn, m := newMockGCSConn("")
	if err := conn.Write(context.Background(), "envs/new.tfstate", []byte(`{"version":4}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if string(m.objects["envs/new.tfstate"]) != `{"version":4}` {
		t.Error("Write did not commit the object")
	}
	if m.written["envs/new.tfstate"] != "application/json" {
		t.Errorf("content type = %q, want application/json", m.written["envs/new.tfstate"])
	}

	m.writeErr = errors.New("quota")
	if err := conn.Write(context.Background(), "k", nil); err == nil || !strings.Contains(err.Error(), "failed to write") {
		t.Errorf("write errors must surface: %v", err)
	}
	m.writeErr = nil
	m.closeErr = errors.New("commit failed")
	if err := conn.Write(context.Background(), "k", nil); err == nil || !strings.Contains(err.Error(), "finalize") {
		t.Errorf("close errors must surface as finalize failures: %v", err)
	}
}
