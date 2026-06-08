package capability

// Registry is an in-memory list of registered capabilities assembled at startup.
// It is not safe for concurrent registration; all Register calls happen during
// wiring (router.go) before the scheduler and HTTP server start serving. Lookups
// after startup are read-only and therefore safe.
type Registry struct {
	capabilities []Capability
	byTaskType   map[string]Capability
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		byTaskType: make(map[string]Capability),
	}
}

// Register adds a capability to the registry. A capability that declares a
// TaskType is indexed for scheduler dispatch; the last registration for a given
// task type wins (registration is single-threaded at startup, so this is a
// programming-error guard rather than a runtime race).
func (r *Registry) Register(cap Capability) {
	r.capabilities = append(r.capabilities, cap)
	if cap.TaskType != "" {
		r.byTaskType[cap.TaskType] = cap
	}
}

// List returns all registered capabilities in registration order.
func (r *Registry) List() []Capability {
	out := make([]Capability, len(r.capabilities))
	copy(out, r.capabilities)
	return out
}

// LookupByTaskType returns the capability handling the given scheduled task type
// and true if one is registered, or the zero value and false otherwise.
func (r *Registry) LookupByTaskType(taskType string) (Capability, bool) {
	cap, ok := r.byTaskType[taskType]
	return cap, ok
}

// TaskTypes returns the set of scheduled task types contributed by registered
// capabilities. Built-in scheduler task types are not included.
func (r *Registry) TaskTypes() []string {
	types := make([]string, 0, len(r.byTaskType))
	for _, cap := range r.capabilities {
		if cap.TaskType != "" {
			types = append(types, cap.TaskType)
		}
	}
	return types
}

// Scopes returns the de-duplicated set of RBAC scope strings contributed by all
// registered capabilities, in first-seen order.
func (r *Registry) Scopes() []string {
	seen := make(map[string]bool)
	scopes := make([]string, 0)
	for _, cap := range r.capabilities {
		for _, s := range cap.Scopes {
			if !seen[s] {
				seen[s] = true
				scopes = append(scopes, s)
			}
		}
	}
	return scopes
}
