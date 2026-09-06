package api

// Runner-side CI definitions served by WorkflowTemplate. They run terraform
// plan -detailed-exitcode, summarize resource changes, and POST the result to the
// TSM callback. Cloud credentials stay in the CI provider (preferably via OIDC/
// workload identity) — never in the state manager.
//
// SECURITY: untrusted run inputs reach the shell only through `env:` and are
// referenced quoted ("$VAR"); they are never interpolated into the `run:` text.
// The callback body is built with `jq -n` (never string concatenation), so a
// crafted summary/value cannot break out of the JSON or the shell. The backend
// also validates these inputs before dispatch (see validate.go).

const githubDriftWorkflow = `# .github/workflows/tsm-drift.yml
name: TSM Drift
on:
  workflow_dispatch:
    inputs:
      callback_url:
        description: TSM callback URL
        required: true
        type: string
      callback_token:
        description: TSM callback token
        required: true
        type: string
      working_dir:
        description: Terraform working directory
        required: false
        default: "."
        type: string
permissions:
  id-token: write   # for cloud OIDC (e.g. AWS/Azure/GCP federation)
  contents: read
jobs:
  drift:
    runs-on: ubuntu-latest
    steps:
      - name: Mask callback token in logs
        env:
          CALLBACK_TOKEN: ${{ inputs.callback_token }}
        run: echo "::add-mask::$CALLBACK_TOKEN"
      - uses: actions/checkout@v4
      - uses: hashicorp/setup-terraform@v3
      # Configure cloud credentials here, preferably via OIDC, e.g.:
      # - uses: aws-actions/configure-aws-credentials@v4
      #   with: { role-to-assume: <role-arn>, aws-region: <region> }
      - name: Plan and report drift
        working-directory: ${{ inputs.working_dir }}
        env:
          CALLBACK_URL: ${{ inputs.callback_url }}
          CALLBACK_TOKEN: ${{ inputs.callback_token }}
        run: |
          set -e
          terraform init -input=false
          set +e
          terraform plan -detailed-exitcode -out=tfplan -input=false
          PLAN_EXIT=$?
          set -e
          # 0 = no diff, 2 = a diff. Anything else is a failed plan, and posting a
          # drift result derived from a plan that did not run is worse than posting
          # nothing — the callback is one-shot, so a wrong answer cannot be corrected.
          if [ "$PLAN_EXIT" != "0" ] && [ "$PLAN_EXIT" != "2" ]; then
            echo "terraform plan failed (exit $PLAN_EXIT)" >&2
            exit "$PLAN_EXIT"
          fi
          terraform show -json tfplan > plan.json
          ADD=$(jq '[.resource_changes[]? | select(.change.actions | index("create"))] | length' plan.json)
          CHG=$(jq '[.resource_changes[]? | select(.change.actions | index("update"))] | length' plan.json)
          DEL=$(jq '[.resource_changes[]? | select(.change.actions | index("delete"))] | length' plan.json)
          # DRIFTED is the CONTRACT's definition — (added + changed + destroyed) > 0
          # — not the plan's exit code. terraform plan -detailed-exitcode answers 2 for
          # a non-empty diff of ANY kind, including an output-only change that produces
          # no resource_changes entries at all, so this posted drifted=true for a plan
          # the report action and the /drift/ingest path both call clean, and the
          # dashboard and alerting key off that field. Conformance vector:
          # drifted/output-only-diff.
          DRIFTED=$( [ "$((ADD + CHG + DEL))" -gt 0 ] && echo true || echo false )
          # The row set matches the contract: an entry whose actions are EXACTLY
          # ["no-op"] or EXACTLY ["read"] is skipped, so a refresh-only plan yields no
          # rows rather than one row per data source; and a JSON null entry is ignored
          # rather than emitted as {address: null, actions: null}. Rows are
          # {address, actions} only — this path carries no attribute values and so has
          # nothing to mask. An absent address emits "" rather than null, matching
          # the contract; a wrong-TYPED address is still copied through, which the
          # corpus states. Conformance vectors: skip/read-exactly,
          # drifted/refresh-only, shape/null-entry-in-resource-changes.
          JQ_ROWS='[.resource_changes[]? | select(type == "object")
            | select(.change.actions != ["no-op"] and .change.actions != ["read"])
            | {address: (.address // ""), actions: .change.actions}]'
          # Bounded like every other emitted value. The counts above are NOT capped, so a
          # capped summary still reports drift truthfully — the counts are the signal, the
          # rows are the detail — and omitted_entries says how much was left out, so a
          # consumer can tell "no more drift" from "we stopped looking". 500 is the
          # contract's limit, declared in its conformance corpus.
          SUMMARY=$(jq -c "$JQ_ROWS"' | .[0:500]' plan.json)
          OMITTED_ENTRIES=$(jq "$JQ_ROWS"' | length - 500 | if . > 0 then . else 0 end' plan.json)
          TRUNCATED=$( [ "$OMITTED_ENTRIES" -gt 0 ] && echo true || echo false )
          # A document that is not a plan at all — a truncated terraform show, the wrong
          # file, a broken step — used to post the SAME clean result as a verified-clean
          # plan. Nothing downstream could tell them apart, which is a false negative on
          # the signal this job exists to produce.
          UNPARSEABLE=$(jq -c 'if (type == "object") and ((.resource_changes|type) == "array")
            then false else true end' plan.json)
          # A change that would emit attribute values and carries NEITHER sensitivity
          # mirror was masked by nothing at all. This path emits no attribute values, so
          # nothing leaks here — but the flag is computed from the same plan shape as the
          # other producers, so an operator gets the same warning whichever one ran.
          UNMASKED=$(jq -c '[.resource_changes[]? | select(type == "object")
            | select(.change.actions != ["no-op"] and .change.actions != ["read"])
            | select((.change.before|type) == "object" and (.change.after|type) == "object")
            | select(.change.before_sensitive == null and .change.after_sensitive == null)]
            | length > 0' plan.json)
          # Module provenance (optional): the configuration's module calls plus the
          # resolved module lockfile, so TSM records which registry modules this
          # state uses and how fresh they are. Both ride the existing jq -n payload
          # via --argjson (never string-concatenated), and are ignored by older servers.
          # ONE scrubber, used by BOTH provenance fields below. They carry the same
          # addresses — modules.json is terraform's resolved view of the very source
          # arguments the configuration block reports — so defining the redaction
          # once is what stops them drifting apart, exactly as the report action
          # reaches the contract's scrubber from both.
          #   scrub: URL userinfo (git::https://x-access-token:ghp_…@…) and every
          #          go-getter query parameter except ref (sshkey=, token=,
          #          X-Amz-Signature=) are replaced with (redacted).
          #   cap:   300 code points + U+2026, matching fmt().
          JQ_REDACT='
            def scrub: sub("://[^/?#@]*@"; "://(redacted)@")
              | . as $s | ($s|index("?")) as $q
              | if $q == null then $s
                else $s[0:$q] + "?" + ($s[$q+1:] | split("&")
                  | map((index("=")) as $e
                        | if $e == null or .[0:$e] == "ref" then . else .[0:$e] + "=(redacted)" end)
                  | join("&"))
                end;
            def cap: if (length > 300) then .[0:300] + "…" else . end;'
          MODULE_CALLS=$(jq -c "$JQ_REDACT"'
            # Module provenance is PROJECTED, never forwarded. The plan configuration
            # block carries NO terraform sensitivity metadata, so relaying the raw
            # module_calls subtree ships expressions.*.constant_value (every literal
            # module argument, e.g. a hardcoded password) and the nested module tree
            # in cleartext. Emit only the two fields the server reads, with source
            # credentials scrubbed and every string capped — matching
            # moduleCallsPlan() in @4cloudguru/terraform-drift-contract.
            def proj: if type == "object" then
                  (if (.source|type) == "string" then {source: (.source|scrub|cap)} else {} end)
                + (if (.version_constraint|type) == "string" then {version_constraint: (.version_constraint|cap)} else {} end)
              else {} end;
            (.configuration.root_module.module_calls) as $raw
            | (if ($raw|type) == "object" then $raw else {} end) as $calls
            | ($calls|keys) as $names
            | (reduce $names[0:100][] as $k ({m:{}, t:(($names|length)>100)};
                 ($k|cap) as $f
                 | if (.m|has($f)) then .t = true else .m[$f] = ($calls[$k]|proj) end)) as $o
            | {configuration:{root_module:({module_calls:$o.m}
                + (if $o.t then {module_calls_truncated:true} else {} end))}}' plan.json)
          # The lockfile is PROJECTED too, for the same reason: terraform records
          # each module's resolved Source verbatim, so a private git module puts the
          # very credential scrubbed out of module_calls above back into the payload
          # one line later. Keep only Source + Version (all ParseModuleLocks reads)
          # and Key (which names the call); Dir — the runner-local checkout path —
          # and any field a later terraform adds are dropped by construction.
          # Matches projectModuleLocks() in sethbacon/terraform-drift-report.
          MODULE_LOCKS=$( [ -f .terraform/modules/modules.json ] \
            && jq -c "$JQ_REDACT"'
              {Modules: [ .Modules[]? | (if type == "object" then . else {} end)
                | (if (.Key|type) == "string" then {Key} else {} end)
                + (if (.Source|type) == "string" then {Source: (.Source|scrub|cap)} else {} end)
                + (if (.Version|type) == "string" then {Version} else {} end) ]}' \
              .terraform/modules/modules.json \
            || echo null )
          PAYLOAD=$(jq -n \
            --argjson added "$ADD" --argjson changed "$CHG" --argjson destroyed "$DEL" \
            --argjson drifted "$DRIFTED" --argjson summary "$SUMMARY" \
            --argjson unparseable "$UNPARSEABLE" --argjson unmasked "$UNMASKED" \
            --argjson truncated "$TRUNCATED" --argjson omitted_entries "$OMITTED_ENTRIES" \
            --argjson plan "$MODULE_CALLS" --argjson module_locks "$MODULE_LOCKS" \
            --arg detail "github run $GITHUB_RUN_ID" \
            '{status:"completed", added:$added, changed:$changed, destroyed:$destroyed, drifted:$drifted, unparseable:$unparseable, unmasked:$unmasked, truncated:$truncated, omitted_entries:$omitted_entries, omitted_attrs:0, summary:$summary, plan:$plan, module_locks:$module_locks, detail:$detail}')
          curl -sf -X POST "$CALLBACK_URL" \
            -H "Content-Type: application/json" \
            -H "X-TSM-Callback-Token: $CALLBACK_TOKEN" \
            -d "$PAYLOAD"
`

const azureDriftPipeline = `# azure-pipelines-tsm-drift.yml  (Azure DevOps)
parameters:
  - name: callback_url
    type: string
  - name: callback_token
    type: string
  - name: working_dir
    type: string
    default: "."
trigger: none
pool:
  vmImage: ubuntu-latest
steps:
  - checkout: self
  - bash: echo "##vso[task.setvariable variable=TSM_CALLBACK_TOKEN;issecret=true]$CALLBACK_TOKEN"
    displayName: Mask callback token in logs
    env:
      CALLBACK_TOKEN: ${{ parameters.callback_token }}
  - task: TerraformInstaller@1
    inputs:
      terraformVersion: latest
  # Configure cloud credentials here (service connection / workload identity).
  - bash: |
      set -e
      cd "$WORKING_DIR"
      terraform init -input=false
      set +e
      terraform plan -detailed-exitcode -out=tfplan -input=false
      PLAN_EXIT=$?
      set -e
      # 0 = no diff, 2 = a diff. Anything else is a failed plan, and posting a
      # drift result derived from a plan that did not run is worse than posting
      # nothing — the callback is one-shot, so a wrong answer cannot be corrected.
      if [ "$PLAN_EXIT" != "0" ] && [ "$PLAN_EXIT" != "2" ]; then
        echo "terraform plan failed (exit $PLAN_EXIT)" >&2
        exit "$PLAN_EXIT"
      fi
      terraform show -json tfplan > plan.json
      ADD=$(jq '[.resource_changes[]? | select(.change.actions | index("create"))] | length' plan.json)
      CHG=$(jq '[.resource_changes[]? | select(.change.actions | index("update"))] | length' plan.json)
      DEL=$(jq '[.resource_changes[]? | select(.change.actions | index("delete"))] | length' plan.json)
      # DRIFTED is the CONTRACT's definition — (added + changed + destroyed) > 0
      # — not the plan's exit code. terraform plan -detailed-exitcode answers 2 for
      # a non-empty diff of ANY kind, including an output-only change that produces
      # no resource_changes entries at all, so this posted drifted=true for a plan
      # the report action and the /drift/ingest path both call clean, and the
      # dashboard and alerting key off that field. Conformance vector:
      # drifted/output-only-diff.
      DRIFTED=$( [ "$((ADD + CHG + DEL))" -gt 0 ] && echo true || echo false )
      # The row set matches the contract: an entry whose actions are EXACTLY
      # ["no-op"] or EXACTLY ["read"] is skipped, so a refresh-only plan yields no
      # rows rather than one row per data source; and a JSON null entry is ignored
      # rather than emitted as {address: null, actions: null}. Rows are
      # {address, actions} only — this path carries no attribute values and so has
      # nothing to mask. An absent address emits "" rather than null, matching
      # the contract; a wrong-TYPED address is still copied through, which the
      # corpus states. Conformance vectors: skip/read-exactly,
      # drifted/refresh-only, shape/null-entry-in-resource-changes.
      JQ_ROWS='[.resource_changes[]? | select(type == "object")
        | select(.change.actions != ["no-op"] and .change.actions != ["read"])
        | {address: (.address // ""), actions: .change.actions}]'
      # Bounded like every other emitted value. The counts above are NOT capped, so a
      # capped summary still reports drift truthfully — the counts are the signal, the
      # rows are the detail — and omitted_entries says how much was left out, so a
      # consumer can tell "no more drift" from "we stopped looking". 500 is the
      # contract's limit, declared in its conformance corpus.
      SUMMARY=$(jq -c "$JQ_ROWS"' | .[0:500]' plan.json)
      OMITTED_ENTRIES=$(jq "$JQ_ROWS"' | length - 500 | if . > 0 then . else 0 end' plan.json)
      TRUNCATED=$( [ "$OMITTED_ENTRIES" -gt 0 ] && echo true || echo false )
      # A document that is not a plan at all — a truncated terraform show, the wrong
      # file, a broken step — used to post the SAME clean result as a verified-clean
      # plan. Nothing downstream could tell them apart, which is a false negative on
      # the signal this job exists to produce.
      UNPARSEABLE=$(jq -c 'if (type == "object") and ((.resource_changes|type) == "array")
        then false else true end' plan.json)
      # A change that would emit attribute values and carries NEITHER sensitivity
      # mirror was masked by nothing at all. This path emits no attribute values, so
      # nothing leaks here — but the flag is computed from the same plan shape as the
      # other producers, so an operator gets the same warning whichever one ran.
      UNMASKED=$(jq -c '[.resource_changes[]? | select(type == "object")
        | select(.change.actions != ["no-op"] and .change.actions != ["read"])
        | select((.change.before|type) == "object" and (.change.after|type) == "object")
        | select(.change.before_sensitive == null and .change.after_sensitive == null)]
        | length > 0' plan.json)
      # Module provenance (optional): the configuration's module calls plus the
      # resolved module lockfile, so TSM records which registry modules this state
      # uses and how fresh they are. Both ride the existing jq -n payload via
      # --argjson (never string-concatenated), and are ignored by older servers.
      # ONE scrubber, used by BOTH provenance fields below. They carry the same
      # addresses — modules.json is terraform's resolved view of the very source
      # arguments the configuration block reports — so defining the redaction once
      # is what stops them drifting apart, exactly as the report task reaches the
      # contract's scrubber from both.
      #   scrub: URL userinfo (git::https://x-access-token:ghp_…@…) and every
      #          go-getter query parameter except ref (sshkey=, token=,
      #          X-Amz-Signature=) are replaced with (redacted).
      #   cap:   300 code points + U+2026, matching fmt().
      JQ_REDACT='
        def scrub: sub("://[^/?#@]*@"; "://(redacted)@")
          | . as $s | ($s|index("?")) as $q
          | if $q == null then $s
            else $s[0:$q] + "?" + ($s[$q+1:] | split("&")
              | map((index("=")) as $e
                    | if $e == null or .[0:$e] == "ref" then . else .[0:$e] + "=(redacted)" end)
              | join("&"))
            end;
        def cap: if (length > 300) then .[0:300] + "…" else . end;'
      MODULE_CALLS=$(jq -c "$JQ_REDACT"'
        # Module provenance is PROJECTED, never forwarded. The plan configuration
        # block carries NO terraform sensitivity metadata, so relaying the raw
        # module_calls subtree ships expressions.*.constant_value (every literal
        # module argument, e.g. a hardcoded password) and the nested module tree
        # in cleartext. Emit only the two fields the server reads, with source
        # credentials scrubbed and every string capped — matching
        # moduleCallsPlan() in @4cloudguru/terraform-drift-contract.
        def proj: if type == "object" then
              (if (.source|type) == "string" then {source: (.source|scrub|cap)} else {} end)
            + (if (.version_constraint|type) == "string" then {version_constraint: (.version_constraint|cap)} else {} end)
          else {} end;
        (.configuration.root_module.module_calls) as $raw
        | (if ($raw|type) == "object" then $raw else {} end) as $calls
        | ($calls|keys) as $names
        | (reduce $names[0:100][] as $k ({m:{}, t:(($names|length)>100)};
             ($k|cap) as $f
             | if (.m|has($f)) then .t = true else .m[$f] = ($calls[$k]|proj) end)) as $o
        | {configuration:{root_module:({module_calls:$o.m}
            + (if $o.t then {module_calls_truncated:true} else {} end))}}' plan.json)
      # The lockfile is PROJECTED too, for the same reason: terraform records each
      # module's resolved Source verbatim, so a private git module puts the very
      # credential scrubbed out of module_calls above back into the payload one
      # line later. Keep only Source + Version (all ParseModuleLocks reads) and Key
      # (which names the call); Dir — the runner-local checkout path — and any
      # field a later terraform adds are dropped by construction. Matches
      # projectModuleLocks() in sethbacon/terraform-drift-report.
      MODULE_LOCKS=$( [ -f .terraform/modules/modules.json ] \
        && jq -c "$JQ_REDACT"'
          {Modules: [ .Modules[]? | (if type == "object" then . else {} end)
            | (if (.Key|type) == "string" then {Key} else {} end)
            + (if (.Source|type) == "string" then {Source: (.Source|scrub|cap)} else {} end)
            + (if (.Version|type) == "string" then {Version} else {} end) ]}' \
          .terraform/modules/modules.json \
        || echo null )
      PAYLOAD=$(jq -n \
        --argjson added "$ADD" --argjson changed "$CHG" --argjson destroyed "$DEL" \
        --argjson drifted "$DRIFTED" --argjson summary "$SUMMARY" \
        --argjson unparseable "$UNPARSEABLE" --argjson unmasked "$UNMASKED" \
        --argjson truncated "$TRUNCATED" --argjson omitted_entries "$OMITTED_ENTRIES" \
        --argjson plan "$MODULE_CALLS" --argjson module_locks "$MODULE_LOCKS" \
        --arg detail "azdo build $BUILD_BUILDID" \
        '{status:"completed", added:$added, changed:$changed, destroyed:$destroyed, drifted:$drifted, unparseable:$unparseable, unmasked:$unmasked, truncated:$truncated, omitted_entries:$omitted_entries, omitted_attrs:0, summary:$summary, plan:$plan, module_locks:$module_locks, detail:$detail}')
      curl -sf -X POST "$CALLBACK_URL" \
        -H "Content-Type: application/json" \
        -H "X-TSM-Callback-Token: $CALLBACK_TOKEN" \
        -d "$PAYLOAD"
    displayName: Plan and report drift
    env:
      CALLBACK_URL: ${{ parameters.callback_url }}
      CALLBACK_TOKEN: ${{ parameters.callback_token }}
      WORKING_DIR: ${{ parameters.working_dir }}
`

// Suite variants (profile="suite") use the published Terraform-suite CI
// components instead of inline jq: the terraform-drift-report GitHub Action and
// the PipelineTerraformDriftReport Azure DevOps task. Both bundle the same
// terraform-drift-contract parser as the /drift/ingest path, so the result is
// identical — but there is no jq dependency, no shell-built JSON, and the runner
// works on Windows agents too. The dispatch inputs are unchanged, so these are
// drop-in alternatives to the built-in templates above.

const githubDriftWorkflowSuite = `# .github/workflows/tsm-drift.yml  (Terraform-suite actions)
name: TSM Drift
on:
  workflow_dispatch:
    inputs:
      callback_url:
        description: TSM callback URL
        required: true
        type: string
      callback_token:
        description: TSM callback token
        required: true
        type: string
      working_dir:
        description: Terraform working directory
        required: false
        default: "."
        type: string
permissions:
  id-token: write   # for cloud OIDC (e.g. AWS/Azure/GCP federation)
  contents: read
jobs:
  drift:
    runs-on: ubuntu-latest
    steps:
      - name: Mask callback token in logs
        env:
          CALLBACK_TOKEN: ${{ inputs.callback_token }}
        run: echo "::add-mask::$CALLBACK_TOKEN"
      - uses: actions/checkout@v4
      - uses: sethbacon/setup-terraform-hardened@v1
        with:
          binary: terraform
          version: latest          # ### EDIT: pin if the app's state needs it
          require-checksum: "true"
      # Configure cloud credentials here, preferably via OIDC, e.g.:
      # - uses: aws-actions/configure-aws-credentials@v4
      #   with: { role-to-assume: <role-arn>, aws-region: <region> }
      - name: Plan
        working-directory: ${{ inputs.working_dir }}
        run: |
          set -e
          terraform init -input=false
          set +e
          terraform plan -detailed-exitcode -out=tfplan -input=false
          set -e
          terraform show -json tfplan > plan.json
      - name: Report drift to TSM
        uses: sethbacon/terraform-drift-report@v1
        with:
          plan-json-file: ${{ inputs.working_dir }}/plan.json
          module-manifest: ${{ inputs.working_dir }}/.terraform/modules/modules.json
          detail: github run ${{ github.run_id }}
          callback-url: ${{ inputs.callback_url }}
          callback-token: ${{ inputs.callback_token }}
`

const azureDriftPipelineSuite = `# azure-pipelines-tsm-drift.yml  (Azure DevOps — Terraform-suite extension)
parameters:
  - name: callback_url
    type: string
  - name: callback_token
    type: string
  - name: working_dir
    type: string
    default: "."
trigger: none
pool:
  vmImage: ubuntu-latest
steps:
  - checkout: self
  - bash: echo "##vso[task.setvariable variable=TSM_CALLBACK_TOKEN;issecret=true]$CALLBACK_TOKEN"
    displayName: Mask callback token in logs
    env:
      CALLBACK_TOKEN: ${{ parameters.callback_token }}
  - task: PipelineTerraformInstaller@1
    displayName: Install Terraform
    inputs:
      terraformVersion: latest          # ### EDIT: pin if the app's state needs it
  - task: PipelineTerraformTask@5
    displayName: terraform init
    inputs:
      provider: azurerm                 # ### EDIT for your cloud (aws|gcp|oci)
      command: init
      workingDirectory: ${{ parameters.working_dir }}
      # ### EDIT: configure your state backend (backendType + backend* inputs).
  - task: PipelineTerraformTask@5
    displayName: terraform plan
    continueOnError: true               # exit code 2 (drift) must not fail the job
    inputs:
      provider: azurerm                 # ### EDIT for your cloud
      command: plan
      workingDirectory: ${{ parameters.working_dir }}
      commandOptions: -detailed-exitcode -out=tfplan -input=false
      # ### EDIT: environmentServiceNameAzureRM (or AWS/GCP/OCI) service connection.
  - bash: terraform show -json tfplan > plan.json
    displayName: terraform show -json
    workingDirectory: ${{ parameters.working_dir }}
  - task: PipelineTerraformDriftReport@1
    displayName: Report drift to TSM
    inputs:
      planJsonFile: ${{ parameters.working_dir }}/plan.json
      moduleManifest: ${{ parameters.working_dir }}/.terraform/modules/modules.json
      detail: azdo build $(Build.BuildId)
      callbackUrl: ${{ parameters.callback_url }}
      callbackToken: ${{ parameters.callback_token }}
`

// azureDriftPipelineFanOut is the repo-level fan-out profile
// (drift-fleet-scale.md Phase 1, task 1.4): one ADO run plans N targets
// (states) sequentially and reports each one's result to its OWN callback
// url/token, so one broken app cannot stop the rest and a partial run still
// reports what it managed. Built on the same Terraform-suite CI components as
// the "suite" profile (PipelineTerraformInstaller@1, PipelineTerraformTask@5,
// PipelineTerraformDriftReport@1) -- no jq dependency, works on Windows agents
// too -- with the per-app loop this profile adds on top.
//
// `targets` is a `type: object` template parameter (design decision #4.3):
// TSM sends it as one compact JSON array only when dispatching 2+ targets, and
// ADO's Pipelines API coerces the JSON-string `templateParameters.targets`
// value into this object parameter at compile time (spike 1.0(a), assumed --
// NOT yet verified against a live ADO run; if that coercion turns out not to
// hold, the documented fallback is `targets` as `type: string` plus a
// runtime-parsed `working_dirs` companion, which only touches this file and
// the dispatch-side `targets` JSON encoding, not the callback contract).
//
// FAILURE SEMANTICS (also stated in drift-fleet-scale.md 1.4): an app whose
// own steps ran but failed gets an immediate `failed` via its failure-report
// step below, using ITS OWN token. An app whose steps were never scheduled at
// all (the job was cancelled or timed out after app k) stays "dispatched"
// until the reconciler expires it at TSM_DRIFT_RUN_TTL (default 2h) -- that is
// intentional, the reconciler is the backstop, and operators with short drift
// jobs may lower TSM_DRIFT_RUN_TTL to ~1h rather than adding job-level cleanup
// steps here.
//
// Empty `targets` (the default, and every non-fan-out dispatch) behaves
// EXACTLY like the "suite" profile: one app, the legacy three parameters,
// guarded by `${{ if eq(length(parameters.targets), 0) }}`.
//
// PER-TARGET CALLBACK TOKEN (drift-fleet-scale.md Phase 1b item 3; spike
// 1.0(b), run 2026-09-05): each target's callback token is NOT a `t.*` field
// -- every `t.*` reference here is compiled verbatim into finalYaml, which is
// exactly how the spike found `${{ t.callback_token }}` exposed (a `type:
// string` parameter is compiled identically, so that is not a fallback
// either). Instead each token arrives as an Azure DevOps secret RUN variable
// named `cb_token_<safe_dir>` in the Runs API request's "variables" bag
// (dispatchDriftBatch's fanOutVariables), resolved only at RUN time and
// referenced here via the `$(cb_token_${{ replace(t.working_dir, '/', '_') }})`
// macro -- ADO compiles only the variable NAME (from working_dir, which is
// not secret); the VALUE is substituted by the agent, never compiled. See
// FanOutCallbackTokenVariableName in drift.go for the Go half of that name
// derivation, which must agree with this template's `replace(...)`
// expression byte for byte.
//
// PER-TARGET SERVICE CONNECTION (drift-fleet-scale.md Phase 1b item 3): the
// per-app WIF service connection also has to resolve at compile time, so it
// travels inside `t.params` (DriftTargetItem.Params, an opaque validated
// pass-through) and is bound here as `${{ t.params.service_connection }}`.
const azureDriftPipelineFanOut = `# azure-pipelines-tsm-drift-fanout.yml  (Azure DevOps — Terraform-suite extension, repo-level fan-out)
parameters:
  - name: callback_url
    type: string
  - name: callback_token
    type: string
  - name: working_dir
    type: string
    default: "."
  - name: targets
    type: object
    default: []
trigger: none
pool:
  vmImage: ubuntu-latest
steps:
  - checkout: self
  - bash: echo "##vso[task.setvariable variable=TSM_CALLBACK_TOKEN;issecret=true]$CALLBACK_TOKEN"
    displayName: Mask callback token in logs
    env:
      CALLBACK_TOKEN: ${{ parameters.callback_token }}
  - task: PipelineTerraformInstaller@1
    displayName: Install Terraform
    inputs:
      terraformVersion: latest          # ### EDIT: pin if the fleet needs it
  # No targets supplied: behave EXACTLY like the non-fan-out drift pipeline --
  # one app, the legacy three parameters. This is the byte-identical back-compat
  # path every existing schedule and every hand-dispatched run takes today.
  - ${{ if eq(length(parameters.targets), 0) }}:
    - task: PipelineTerraformTask@5
      displayName: terraform init
      inputs:
        provider: azurerm               # ### EDIT for your cloud (aws|gcp|oci)
        command: init
        workingDirectory: ${{ parameters.working_dir }}
        # ### EDIT: configure your state backend (backendType + backend* inputs).
    - task: PipelineTerraformTask@5
      displayName: terraform plan
      continueOnError: true             # exit code 2 (drift) must not fail the job
      inputs:
        provider: azurerm
        command: plan
        workingDirectory: ${{ parameters.working_dir }}
        commandOptions: -detailed-exitcode -out=tfplan -input=false
        # ### EDIT: environmentServiceNameAzureRM (or AWS/GCP/OCI) service connection.
    - bash: terraform show -json tfplan > plan.json
      displayName: terraform show -json
      workingDirectory: ${{ parameters.working_dir }}
    - task: PipelineTerraformDriftReport@1
      displayName: Report drift to TSM
      inputs:
        planJsonFile: ${{ parameters.working_dir }}/plan.json
        moduleManifest: ${{ parameters.working_dir }}/.terraform/modules/modules.json
        detail: azdo build $(Build.BuildId)
        callbackUrl: ${{ parameters.callback_url }}
        callbackToken: ${{ parameters.callback_token }}
  # 2+ targets: one job plans every app in sequence, each with its OWN
  # callback url/token -- reported independently, so one broken app does not
  # stop the rest. Every step below is continueOnError for exactly that reason.
  #
  # Each target's callback token is never a t.* field (those compile into
  # finalYaml verbatim -- see the header comment). It is instead the secret
  # run variable cb_token_<safe_dir>, set by TSM in the Runs API request that
  # started this run, and referenced below only via the
  # $(cb_token_${{ replace(t.working_dir, '/', '_') }}) macro -- resolved by
  # the agent at run time, with masking already registered, so no separate
  # "mask this token" step is needed here (unlike the top-level
  # parameters.callback_token above, which IS a compile-time value and so
  # still needs one).
  - ${{ each t in parameters.targets }}:
    - task: PipelineTerraformTask@5
      displayName: "terraform init: ${{ t.working_dir }}"
      continueOnError: true
      inputs:
        provider: azurerm                 # ### EDIT for your cloud (aws|gcp|oci)
        command: init
        workingDirectory: ${{ t.working_dir }}
        environmentServiceNameAzureRM: ${{ t.params.service_connection }}
        # ### EDIT: configure your state backend; this app's state key is
        # ${{ t.state_key }} -- wire it into the backend* inputs (e.g. backendAzureRmKey).
    - task: PipelineTerraformTask@5
      displayName: "terraform plan: ${{ t.working_dir }}"
      continueOnError: true              # exit code 2 (drift) must not fail the job
      inputs:
        provider: azurerm
        command: plan
        workingDirectory: ${{ t.working_dir }}
        commandOptions: -detailed-exitcode -out=tfplan -input=false
        environmentServiceNameAzureRM: ${{ t.params.service_connection }}
    - bash: terraform show -json tfplan > plan.json
      displayName: "terraform show -json: ${{ t.working_dir }}"
      continueOnError: true
      workingDirectory: ${{ t.working_dir }}
    - task: PipelineTerraformDriftReport@1
      displayName: "Report drift to TSM: ${{ t.working_dir }}"
      continueOnError: true
      inputs:
        planJsonFile: ${{ t.working_dir }}/plan.json
        moduleManifest: ${{ t.working_dir }}/.terraform/modules/modules.json
        detail: azdo build $(Build.BuildId)
        callbackUrl: ${{ t.callback_url }}
        callbackToken: $(cb_token_${{ replace(t.working_dir, '/', '_') }})
    # Marks this app reported so the failure step below (which runs on EVERY
    # app, always()) does not double-report a happy path.
    - bash: echo "##vso[task.setvariable variable=reported_${{ replace(t.working_dir, '/', '_') }}]true"
      displayName: "Mark reported: ${{ t.working_dir }}"
      condition: always()
      continueOnError: true
    # FAILURE SEMANTICS: this app's own steps ran but one of them failed (init,
    # a plan failure that is NOT just drift, show, or the report task itself)
    # before the marker above could run. Posts an explicit "failed" with THIS
    # app's own one-shot token; a per-target variable condition (never a
    # job-level "always failed" condition, which continueOnError above would
    # prevent from ever firing) is what makes this per-app rather than
    # per-job. HTTP 409 means this app's happy-path report already landed (a
    # benign race); the code is only logged, not treated as an error.
    - bash: |
        PAYLOAD=$(jq -n --arg detail "azdo build $(Build.BuildId): step failed before reporting" \
          '{status:"failed", detail:$detail}')
        CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$CB_URL" \
          -H "Content-Type: application/json" \
          -H "X-TSM-Callback-Token: $CB_TOKEN" \
          -d "$PAYLOAD")
        echo "failure report -> HTTP $CODE (200=recorded, 409=already reported)"
      displayName: "Failure report: ${{ t.working_dir }}"
      condition: and(always(), ne(variables['reported_${{ replace(t.working_dir, '/', '_') }}'], 'true'))
      continueOnError: true
      env:
        CB_URL: ${{ t.callback_url }}
        CB_TOKEN: $(cb_token_${{ replace(t.working_dir, '/', '_') }})
    # Reset for the next app: keep the provider plugin cache (.terraform),
    # drop everything else terraform init/plan wrote for this app.
    - bash: git checkout -- . && git clean -fd -e .terraform
      displayName: "Reset workspace: ${{ t.working_dir }}"
      continueOnError: true
`
