# Naming compliance policy, expressed in Rego.
#
# Mirrors rules.CheckNaming: the workspace name is checked against
# workspace_pattern, and every key of result.resources_by_type is checked
# against resource_pattern. Both patterns are RE2 and matched unanchored, the
# same as Go's regexp.MatchString. Empty patterns are skipped.
#
# input = {
#   "policy_config": { "workspace_pattern": "..", "resource_pattern": ".." },
#   "result":        { "workspace_name": "..", "resources_by_type": {..} }
# }
package compliance.naming

workspace_pattern := object.get(input.policy_config, "workspace_pattern", "")

resource_pattern := object.get(input.policy_config, "resource_pattern", "")

workspace_name := object.get(input.result, "workspace_name", "")

resources := object.get(input.result, "resources_by_type", {})

# Workspace name violates its pattern.
violations contains v if {
	workspace_pattern != ""
	not regex.match(workspace_pattern, workspace_name)
	v := {
		"type": "workspace_name",
		"name": workspace_name,
		"pattern": workspace_pattern,
		"message": sprintf("Workspace name '%s' does not match required pattern: %s", [workspace_name, workspace_pattern]),
	}
}

# A resource name violates its pattern.
violations contains v if {
	resource_pattern != ""
	some resource_name, _ in resources
	not regex.match(resource_pattern, resource_name)
	v := {
		"type": "resource_name",
		"name": resource_name,
		"pattern": resource_pattern,
		"message": sprintf("Resource name '%s' does not match required pattern: %s", [resource_name, resource_pattern]),
	}
}
