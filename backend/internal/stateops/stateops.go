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
	"strconv"
	"strings"
)

// RemoveResource removes a resource from state, like `terraform state rm`.
// address is "type.name" with optional "module.X." prefixes. When the address
// carries a for_each/count instance index (e.g. `type.name["a"]` or `type.name[0]`)
// only that single instance is removed — and the resource block is dropped once
// its last instance leaves; without an index the whole resource (every instance)
// is removed.
func RemoveResource(raw []byte, address string) ([]byte, error) {
	state, resources, err := decode(raw)
	if err != nil {
		return nil, err
	}
	mod, typ, name, index, err := parseAddress(address)
	if err != nil {
		return nil, err
	}

	kept := make([]map[string]json.RawMessage, 0, len(resources))
	found := false
	for _, r := range resources {
		if !resourceMatches(r, mod, typ, name) {
			kept = append(kept, r)
			continue
		}
		if index == nil {
			// No index: remove the whole resource (all instances).
			found = true
			continue
		}
		// Indexed: remove only the matching instance, dropping the block if empty.
		_, remaining, ok, err := popInstance(r, index)
		if err != nil {
			return nil, err
		}
		if ok {
			found = true
			if remaining == 0 {
				continue
			}
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
// When either address carries an instance index the move is scoped to that single
// for_each/count instance (see moveInstance); both endpoints must then be indexed.
func MoveResource(raw []byte, fromAddr, toAddr string) ([]byte, error) {
	state, resources, err := decode(raw)
	if err != nil {
		return nil, err
	}
	fromMod, fromType, fromName, fromIdx, err := parseAddress(fromAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid source address: %w", err)
	}
	toMod, toType, toName, toIdx, err := parseAddress(toAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid target address: %w", err)
	}

	// An index on either endpoint means a single-instance move; delegate so a
	// whole-resource move can never be mistaken for (or silently swallow) one.
	if fromIdx != nil || toIdx != nil {
		return moveInstance(state, resources,
			fromMod, fromType, fromName, fromIdx,
			toMod, toType, toName, toIdx, fromAddr, toAddr)
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
// type, name, and (optional) instance index. Leading "module.NAME" pairs form the
// module path. A trailing instance index on the resource name — `["key"]` for
// for_each or `[0]` for count — is returned as index (a string or int); it is nil
// when absent.
func parseAddress(addr string) (module, typ, name string, index any, err error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", "", "", nil, fmt.Errorf("empty address")
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
		return "", "", "", nil, fmt.Errorf("address %q must be type.name (optionally module.X.-prefixed)", addr)
	}
	typ = rest[0]
	name = strings.Join(rest[1:], ".")
	if idx := strings.IndexByte(name, '['); idx >= 0 {
		index, err = parseIndexKey(name[idx:])
		if err != nil {
			return "", "", "", nil, err
		}
		name = name[:idx]
	}
	return strings.Join(modParts, "."), typ, name, index, nil
}

// parseIndexKey parses a bracketed instance index such as `["key"]` (for_each) or
// `[0]` (count) into its Go value: a string for_each key or an int count index.
func parseIndexKey(s string) (any, error) {
	if len(s) < 3 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil, fmt.Errorf("malformed instance index %q", s)
	}
	inner := s[1 : len(s)-1]
	if len(inner) >= 2 && inner[0] == '"' {
		var key string
		if err := json.Unmarshal([]byte(inner), &key); err != nil {
			return nil, fmt.Errorf("malformed for_each key %q", inner)
		}
		return key, nil
	}
	n, err := strconv.Atoi(inner)
	if err != nil {
		return nil, fmt.Errorf("malformed count index %q", inner)
	}
	return n, nil
}

// decodeInstances unmarshals a resource block's "instances" array.
func decodeInstances(r map[string]json.RawMessage) ([]map[string]json.RawMessage, error) {
	var insts []map[string]json.RawMessage
	if raw, ok := r["instances"]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &insts); err != nil {
			return nil, fmt.Errorf("invalid instances in state: %w", err)
		}
	}
	return insts, nil
}

// popInstance removes the instance whose index_key matches key from r (writing the
// reduced list back), returning the removed instance, the count still remaining,
// and whether a match was found.
func popInstance(r map[string]json.RawMessage, key any) (inst map[string]json.RawMessage, remaining int, found bool, err error) {
	insts, err := decodeInstances(r)
	if err != nil {
		return nil, 0, false, err
	}
	kept := make([]map[string]json.RawMessage, 0, len(insts))
	for _, in := range insts {
		if !found && indexKeyMatches(in, key) {
			inst, found = in, true
			continue
		}
		kept = append(kept, in)
	}
	if found {
		r["instances"] = mustJSON(kept)
	}
	return inst, len(kept), found, nil
}

// indexKeyMatches reports whether an instance's index_key equals key (a string
// for_each key or an int count index).
func indexKeyMatches(inst map[string]json.RawMessage, key any) bool {
	raw, ok := inst["index_key"]
	if !ok {
		return false
	}
	switch k := key.(type) {
	case string:
		var s string
		return json.Unmarshal(raw, &s) == nil && s == k
	case int:
		var n float64
		return json.Unmarshal(raw, &n) == nil && n == float64(k)
	}
	return false
}

// moveInstance moves a single resource instance (identified by fromIdx) to a new
// address, re-keying it to toIdx. The target resource block is created if absent
// (cloning the source's mode/provider/expansion fields) and the source block is
// dropped once its last instance leaves. Both endpoints must be indexed — a bare,
// whole-resource endpoint is rejected so a single-instance move can never silently
// rewrite an entire resource.
func moveInstance(state map[string]json.RawMessage, resources []map[string]json.RawMessage,
	fromMod, fromType, fromName string, fromIdx any,
	toMod, toType, toName string, toIdx any,
	fromAddr, toAddr string) ([]byte, error) {

	if fromIdx == nil || toIdx == nil {
		return nil, fmt.Errorf("moving a single instance requires an instance index on both the source and target address")
	}

	srcPos, dstPos := -1, -1
	for i, r := range resources {
		if srcPos < 0 && resourceMatches(r, fromMod, fromType, fromName) {
			srcPos = i
		}
		if dstPos < 0 && resourceMatches(r, toMod, toType, toName) {
			dstPos = i
		}
	}
	if srcPos < 0 {
		return nil, fmt.Errorf("resource %q not found in state", fromAddr)
	}
	src := resources[srcPos]

	// Reject a clash with an existing destination instance.
	if dstPos >= 0 {
		insts, err := decodeInstances(resources[dstPos])
		if err != nil {
			return nil, err
		}
		for _, in := range insts {
			if indexKeyMatches(in, toIdx) {
				return nil, fmt.Errorf("target %q already exists in state", toAddr)
			}
		}
	}

	inst, remaining, found, err := popInstance(src, fromIdx)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("resource %q not found in state", fromAddr)
	}
	inst["index_key"] = mustJSON(toIdx)

	if dstPos >= 0 {
		// Append to the existing destination block (re-reads instances so a
		// same-block re-key sees the just-reduced list).
		insts, err := decodeInstances(resources[dstPos])
		if err != nil {
			return nil, err
		}
		resources[dstPos]["instances"] = mustJSON(append(insts, inst))
	} else {
		nb := cloneResourceIdentity(src, toMod, toType, toName)
		nb["instances"] = mustJSON([]map[string]json.RawMessage{inst})
		resources = append(resources, nb)
	}

	// Drop the source block if it is now empty (never when the re-key targeted the
	// same block, where dstPos == srcPos and the instance count is unchanged).
	if remaining == 0 && dstPos != srcPos {
		kept := make([]map[string]json.RawMessage, 0, len(resources))
		for i, r := range resources {
			if i == srcPos {
				continue
			}
			kept = append(kept, r)
		}
		resources = kept
	}
	return encode(state, resources)
}

// cloneResourceIdentity builds a new resource block that copies src's non-identity
// fields (mode, provider, the for_each/count "each" marker, …) and sets the given
// module/type/name. The caller supplies the instances.
func cloneResourceIdentity(src map[string]json.RawMessage, mod, typ, name string) map[string]json.RawMessage {
	nb := make(map[string]json.RawMessage, len(src))
	for k, v := range src {
		switch k {
		case "instances", "module", "type", "name":
			// address fields are set below; instances are supplied by the caller
		default:
			nb[k] = v
		}
	}
	if mod != "" {
		nb["module"] = mustJSON(mod)
	}
	nb["type"] = mustJSON(typ)
	nb["name"] = mustJSON(name)
	return nb
}
