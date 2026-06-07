# Version compliance policy, expressed in Rego.
#
# Mirrors rules.CheckVersion and its compareVersions helper: strip a leading
# "v", split on ".", take the leading integer of each of the first three
# segments (missing segments count as 0), and compare numerically. An unknown
# (empty) actual version is a "version_unknown" violation; a version below the
# minimum is "version_outdated". An empty min_version produces no violations.
#
# input = {
#   "policy_config": { "min_version": ".." },
#   "result":        { "terraform_version": ".." | null }
# }
package compliance.version

min_version := object.get(input.policy_config, "min_version", "")

actual_version := v if {
	raw := object.get(input.result, "terraform_version", null)
	is_string(raw)
	v := raw
}

actual_version := "" if {
	not is_string(object.get(input.result, "terraform_version", null))
}

# Unknown version: min_version is set but the workspace reports no version.
violations contains v if {
	min_version != ""
	actual_version == ""
	v := {
		"type": "version_unknown",
		"min_version": min_version,
		"message": "Terraform version is unknown for this workspace",
	}
}

# Outdated version: a known version that compares below the minimum.
violations contains v if {
	min_version != ""
	actual_version != ""
	compare_versions(actual_version, min_version) < 0
	v := {
		"type": "version_outdated",
		"actual_version": actual_version,
		"min_version": min_version,
		"message": sprintf("Terraform version %s is below minimum required version %s", [actual_version, min_version]),
	}
}

# compare_versions returns -1, 0, or 1 comparing the first three numeric
# components of a and b, matching the Go compareVersions helper. The
# comparison is non-recursive: pad each to three components, find the first
# index at which they differ, and compare there; equal triples return 0.
compare_versions(a, b) := result if {
	pa := padded3(version_parts(a))
	pb := padded3(version_parts(b))
	diff_indices := [i | some i in numbers.range(0, 2); pa[i] != pb[i]]
	count(diff_indices) > 0
	first := min(diff_indices)
	pa[first] < pb[first]
	result := -1
}

compare_versions(a, b) := result if {
	pa := padded3(version_parts(a))
	pb := padded3(version_parts(b))
	diff_indices := [i | some i in numbers.range(0, 2); pa[i] != pb[i]]
	count(diff_indices) > 0
	first := min(diff_indices)
	pa[first] > pb[first]
	result := 1
}

compare_versions(a, b) := 0 if {
	pa := padded3(version_parts(a))
	pb := padded3(version_parts(b))
	diff_indices := [i | some i in numbers.range(0, 2); pa[i] != pb[i]]
	count(diff_indices) == 0
}

# padded3 returns the first three components of parts, padding missing
# positions with 0 to match the Go comparison over three components.
padded3(parts) := [component(parts, 0), component(parts, 1), component(parts, 2)]

# component returns the i-th parsed integer of parts, or 0 when out of range.
component(parts, i) := parts[i] if i < count(parts)

component(parts, i) := 0 if i >= count(parts)

# version_parts strips a leading "v", splits on ".", and parses the leading
# integer of each segment.
version_parts(version) := parts if {
	trimmed := trim_prefix(version, "v")
	segments := split(trimmed, ".")
	parts := [n | some s in segments; n := leading_int(s)]
}

# leading_int returns the integer formed by the leading ASCII digits of s,
# or 0 when s does not start with a digit. This matches the Go parser, which
# accumulates digits until the first non-digit and yields 0 otherwise.
leading_int(s) := n if {
	matches := regex.find_n(`^[0-9]+`, s, 1)
	count(matches) == 1
	n := to_number(matches[0])
}

leading_int(s) := 0 if {
	matches := regex.find_n(`^[0-9]+`, s, 1)
	count(matches) == 0
}
