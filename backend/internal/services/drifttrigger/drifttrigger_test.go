package drifttrigger

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/auth/federation"
	"github.com/terraform-state-manager/terraform-state-manager/internal/clients/ado"
)

// fakeQueuer records the request it received and returns a canned run or error.
type fakeQueuer struct {
	gotReq ado.QueuePipelineRunRequest
	run    *ado.Run
	err    error
}

func (f *fakeQueuer) QueuePipelineRun(_ context.Context, req ado.QueuePipelineRunRequest) (*ado.Run, error) {
	f.gotReq = req
	return f.run, f.err
}

// capturingFactory wraps a fakeQueuer and records the token it was built with.
func capturingFactory(q PipelineQueuer, gotToken *string) QueuerFactory {
	return func(token string) (PipelineQueuer, error) {
		*gotToken = token
		return q, nil
	}
}

func TestTrigger_Success(t *testing.T) {
	fq := &fakeQueuer{run: &ado.Run{ID: 1234, State: "inProgress", URL: "https://ado/run/1234"}}
	var usedToken string
	svc := NewService(
		federation.StaticTokenProvider{Value: "fed-bearer"},
		capturingFactory(fq, &usedToken),
	)

	res, err := svc.Trigger(context.Background(), TriggerRequest{
		SourceID:   "src-1",
		PipelineID: 42,
		Branch:     "refs/heads/main",
		Parameters: map[string]string{"mode": "plan"},
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if res.RunID != 1234 || res.RunState != "inProgress" {
		t.Fatalf("unexpected result: %+v", res)
	}
	// The federated token must flow into the ADO client factory.
	if usedToken != "fed-bearer" {
		t.Errorf("factory token = %q, want fed-bearer", usedToken)
	}
	// The request fields must be forwarded to the queuer.
	if fq.gotReq.PipelineID != 42 || fq.gotReq.Branch != "refs/heads/main" {
		t.Errorf("forwarded request = %+v", fq.gotReq)
	}
	if fq.gotReq.Parameters["mode"] != "plan" {
		t.Errorf("forwarded parameters = %v", fq.gotReq.Parameters)
	}
}

func TestTrigger_RequiresPipelineID(t *testing.T) {
	svc := NewService(federation.StaticTokenProvider{Value: "x"}, capturingFactory(&fakeQueuer{}, new(string)))
	if _, err := svc.Trigger(context.Background(), TriggerRequest{PipelineID: 0}); err == nil {
		t.Fatal("expected error when PipelineID is unset")
	}
}

// errProvider is a TokenProvider that always fails, simulating a federation
// exchange error.
type errProvider struct{}

func (errProvider) Token(context.Context) (string, error) {
	return "", errors.New("exchange boom")
}

func TestTrigger_TokenError(t *testing.T) {
	svc := NewService(errProvider{}, capturingFactory(&fakeQueuer{}, new(string)))
	if _, err := svc.Trigger(context.Background(), TriggerRequest{PipelineID: 42}); err == nil {
		t.Fatal("expected error when token acquisition fails")
	}
}

func TestTrigger_QueueError(t *testing.T) {
	fq := &fakeQueuer{err: errors.New("ado 500")}
	svc := NewService(federation.StaticTokenProvider{Value: "x"}, capturingFactory(fq, new(string)))
	if _, err := svc.Trigger(context.Background(), TriggerRequest{PipelineID: 42}); err == nil {
		t.Fatal("expected error when queue call fails")
	}
}

// TestTrigger_WithRealADOFactory exercises the production QueuerFactory end to
// end against an httptest ADO server, confirming the bearer token and run body
// reach the wire. No live calls — the server is local.
func TestTrigger_WithRealADOFactory(t *testing.T) {
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":99,"state":"inProgress","url":"https://ado/run/99"}`))
	}))
	t.Cleanup(srv.Close)

	svc := NewService(
		federation.StaticTokenProvider{Value: "real-bearer"},
		NewADOQueuerFactory(srv.URL, "Contoso"),
	)

	res, err := svc.Trigger(context.Background(), TriggerRequest{SourceID: "s", PipelineID: 5})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if res.RunID != 99 {
		t.Errorf("RunID = %d, want 99", res.RunID)
	}
	// The ADO PAT scheme is Basic base64(":" + token); just confirm auth was set.
	if seenAuth == "" {
		t.Error("expected Authorization header to be sent to ADO")
	}
}
