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
