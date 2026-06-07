# Tagging compliance policy, expressed in Rego.
#
# Mirrors rules.CheckTagging: for every resource type in
# result.resources_by_type, every required tag that is absent (either because
# the resource carries no tags object or the specific key is missing) yields a
# violation with the same shape and message the Go rule produces.
#
# Tag presence is by KEY EXISTENCE, not value truthiness, to match the Go
# rule's `if _, exists := tags[requiredTag]; !exists` check.
#
# input = {
#   "policy_config": { "required_tags": [ ... ] },
#   "result":        { "resources_by_type": { <type>: { "tags": {..} }, .. } }
# }
package compliance.tagging

required_tags := input.policy_config.required_tags

resources := object.get(input.result, "resources_by_type", {})

# Violation when the resource type has no tags object (absent or non-object),
# so every required tag is missing.
violations contains v if {
	some resource_type
	entry := resources[resource_type]
	is_object(entry)
	not has_tags_object(entry)
	some required_tag in required_tags
	v := violation(resource_type, required_tag)
}

# Violation when the resource type has a tags object that lacks the key.
violations contains v if {
	some resource_type
	entry := resources[resource_type]
	is_object(entry)
	has_tags_object(entry)
	some required_tag in required_tags
	not key_present(entry.tags, required_tag)
	v := violation(resource_type, required_tag)
}

has_tags_object(entry) if is_object(entry.tags)

key_present(tags, key) if _ = tags[key]

violation(resource_type, required_tag) := {
	"resource_type": resource_type,
	"missing_tag": required_tag,
	"message": sprintf("Resource type %s is missing required tag: %s", [resource_type, required_tag]),
}
