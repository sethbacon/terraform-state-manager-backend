package capability

import (
	"context"
	"testing"

	"github.com/terraform-state-manager/terraform-state-manager/internal/db/models"
)

func TestRegistry_RegisterAndList(t *testing.T) {
	r := NewRegistry()
	if got := len(r.List()); got != 0 {
		t.Fatalf("empty registry List() = %d, want 0", got)
	}

	r.Register(Capability{Name: "Alpha", Key: "alpha"})
	r.Register(Capability{Name: "Beta", Key: "beta"})

	list := r.List()
	if len(list) != 2 {
		t.Fatalf("List() = %d, want 2", len(list))
	}
	// Registration order is preserved.
	if list[0].Key != "alpha" || list[1].Key != "beta" {
		t.Errorf("List() order = [%s %s], want [alpha beta]", list[0].Key, list[1].Key)
	}
}

func TestRegistry_LookupByTaskType(t *testing.T) {
	r := NewRegistry()
	called := false
	r.Register(Capability{
		Name:     "Versiontest",
		Key:      "versiontest",
		TaskType: "versiontest",
		TaskHandler: func(_ context.Context, _ *models.ScheduledTask) string {
			called = true
			return models.TaskRunStatusSuccess
		},
	})

	cap, ok := r.LookupByTaskType("versiontest")
	if !ok {
		t.Fatal("LookupByTaskType(versiontest) not found")
	}
	if status := cap.TaskHandler(context.Background(), &models.ScheduledTask{}); status != models.TaskRunStatusSuccess {
		t.Errorf("handler status = %q, want success", status)
	}
	if !called {
		t.Error("handler was not invoked")
	}

	if _, ok := r.LookupByTaskType("nope"); ok {
		t.Error("LookupByTaskType(nope) should not be found")
	}
}

func TestRegistry_TaskTypes(t *testing.T) {
	r := NewRegistry()
	r.Register(Capability{Name: "NoTask", Key: "notask"}) // no task type
	r.Register(Capability{Name: "WithTask", Key: "withtask", TaskType: "withtask"})

	types := r.TaskTypes()
	if len(types) != 1 || types[0] != "withtask" {
		t.Errorf("TaskTypes() = %v, want [withtask]", types)
	}
}

func TestRegistry_Scopes_Deduplicated(t *testing.T) {
	r := NewRegistry()
	r.Register(Capability{Name: "A", Key: "a", Scopes: []string{"foo:admin", "shared:read"}})
	r.Register(Capability{Name: "B", Key: "b", Scopes: []string{"bar:admin", "shared:read"}})

	scopes := r.Scopes()
	want := []string{"foo:admin", "shared:read", "bar:admin"}
	if len(scopes) != len(want) {
		t.Fatalf("Scopes() = %v, want %v", scopes, want)
	}
	for i := range want {
		if scopes[i] != want[i] {
			t.Errorf("Scopes()[%d] = %q, want %q", i, scopes[i], want[i])
		}
	}
}
