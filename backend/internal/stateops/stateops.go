// Package stateops implements structured edits on Terraform state — the
// equivalents of `terraform state rm` and `terraform state mv` — operating on the
// raw state JSON. Top-level keys and every resource field are preserved (the
// state round-trips through generic maps) so only the targeted resource changes;
// the serial is bumped on success. `import` is intentionally not implemented (it
// requires a provider/terraform run, which belongs in the CI/version-lab path).
package stateops

import (
	"encoding/json"
	"fmt"
	"strings"
)

// RemoveResource removes the resource at address (all its instances), like
// `terraform state rm`. address is "type.name" with optional "module.X." prefixes.
func RemoveResource(raw []byte, address string) ([]byte, error) {
	state, resources, err := decode(raw)
	if err != nil {
		return nil, err
	}
	mod, typ, name, err := parseAddress(address)
	if err != nil {
		return nil, err
	}

	kept := make([]map[string]json.RawMessage, 0, len(resources))
	found := false
	for _, r := range resources {
		if resourceMatches(r, mod, typ, name) {
			found = true
			continue
		}
		kept = append(kept, r)
	}
	if !found {
		return nil, fmt.Errorf("resource %q not found in state", address)
	}
	return encode(state, kept)
}

// MoveResource renames the resource at fromAddr to toAddr, like
// `terraform state mv`. It fails if the source is missing or the target exists.
func MoveResource(raw []byte, fromAddr, toAddr string) ([]byte, error) {
	state, resources, err := decode(raw)
	if err != nil {
		return nil, err
	}
	fromMod, fromType, fromName, err := parseAddress(fromAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid source address: %w", err)
	}
	toMod, toType, toName, err := parseAddress(toAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid target address: %w", err)
	}

	var target map[string]json.RawMessage
	for _, r := range resources {
		if resourceMatches(r, toMod, toType, toName) {
			return nil, fmt.Errorf("target %q already exists in state", toAddr)
		}
		if resourceMatches(r, fromMod, fromType, fromName) {
			target = r
		}
	}
	if target == nil {
		return nil, fmt.Errorf("resource %q not found in state", fromAddr)
	}

	if toMod == "" {
		delete(target, "module")
	} else {
		target["module"] = mustJSON(toMod)
	}
	target["type"] = mustJSON(toType)
	target["name"] = mustJSON(toName)
	return encode(state, resources)
}

func decode(raw []byte) (map[string]json.RawMessage, []map[string]json.RawMessage, error) {
	var state map[string]json.RawMessage
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, nil, fmt.Errorf("invalid state JSON: %w", err)
	}
	var resources []map[string]json.RawMessage
	if rawRes, ok := state["resources"]; ok && len(rawRes) > 0 {
		if err := json.Unmarshal(rawRes, &resources); err != nil {
			return nil, nil, fmt.Errorf("invalid resources in state: %w", err)
		}
	}
	return state, resources, nil
}

func encode(state map[string]json.RawMessage, resources []map[string]json.RawMessage) ([]byte, error) {
	resJSON, err := json.Marshal(resources)
	if err != nil {
		return nil, err
	}
	state["resources"] = resJSON
	bumpSerial(state)
	return json.Marshal(state)
}

func bumpSerial(state map[string]json.RawMessage) {
	var serial int64
	if rawSerial, ok := state["serial"]; ok {
		_ = json.Unmarshal(rawSerial, &serial)
	}
	state["serial"] = mustJSON(serial + 1)
}

func resourceMatches(r map[string]json.RawMessage, mod, typ, name string) bool {
	return strField(r, "module") == mod && strField(r, "type") == typ && strField(r, "name") == name
}

func strField(r map[string]json.RawMessage, key string) string {
	var s string
	if v, ok := r[key]; ok {
		_ = json.Unmarshal(v, &s)
	}
	return s
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// parseAddress splits a Terraform resource address into the state's module path,
// type, and name. Leading "module.NAME" pairs form the module path; a trailing
// instance index on the resource name (e.g. "[0]") is ignored (rm/mv act on the
// whole resource).
func parseAddress(addr string) (module, typ, name string, err error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", "", "", fmt.Errorf("empty address")
	}
	parts := strings.Split(addr, ".")
	i := 0
	var modParts []string
	for i+1 < len(parts) && parts[i] == "module" {
		modParts = append(modParts, "module", parts[i+1])
		i += 2
	}
	rest := parts[i:]
	if len(rest) < 2 {
		return "", "", "", fmt.Errorf("address %q must be type.name (optionally module.X.-prefixed)", addr)
	}
	typ = rest[0]
	name = strings.Join(rest[1:], ".")
	if idx := strings.IndexByte(name, '['); idx >= 0 {
		name = name[:idx]
	}
	return strings.Join(modParts, "."), typ, name, nil
}
