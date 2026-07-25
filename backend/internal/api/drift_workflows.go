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
          terraform show -json tfplan > plan.json
          ADD=$(jq '[.resource_changes[]? | select(.change.actions | index("create"))] | length' plan.json)
          CHG=$(jq '[.resource_changes[]? | select(.change.actions | index("update"))] | length' plan.json)
          DEL=$(jq '[.resource_changes[]? | select(.change.actions | index("delete"))] | length' plan.json)
          DRIFTED=$( [ "$PLAN_EXIT" = "2" ] && echo true || echo false )
          SUMMARY=$(jq -c '[.resource_changes[]? | select(.change.actions != ["no-op"]) | {address: .address, actions: .change.actions}]' plan.json)
          # Module provenance (optional): the configuration's module calls plus the
          # resolved module lockfile, so TSM records which registry modules this
          # state uses and how fresh they are. Both ride the existing jq -n payload
          # via --argjson (never string-concatenated), and are ignored by older servers.
          MODULE_CALLS=$(jq -c '{configuration:{root_module:{module_calls:(.configuration.root_module.module_calls // {})}}}' plan.json)
          MODULE_LOCKS=$( [ -f .terraform/modules/modules.json ] && jq -c . .terraform/modules/modules.json || echo null )
          PAYLOAD=$(jq -n \
            --argjson added "$ADD" --argjson changed "$CHG" --argjson destroyed "$DEL" \
            --argjson drifted "$DRIFTED" --argjson summary "$SUMMARY" \
            --argjson plan "$MODULE_CALLS" --argjson module_locks "$MODULE_LOCKS" \
            --arg detail "github run $GITHUB_RUN_ID" \
            '{status:"completed", added:$added, changed:$changed, destroyed:$destroyed, drifted:$drifted, summary:$summary, plan:$plan, module_locks:$module_locks, detail:$detail}')
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
      terraform show -json tfplan > plan.json
      ADD=$(jq '[.resource_changes[]? | select(.change.actions | index("create"))] | length' plan.json)
      CHG=$(jq '[.resource_changes[]? | select(.change.actions | index("update"))] | length' plan.json)
      DEL=$(jq '[.resource_changes[]? | select(.change.actions | index("delete"))] | length' plan.json)
      DRIFTED=$( [ "$PLAN_EXIT" = "2" ] && echo true || echo false )
      SUMMARY=$(jq -c '[.resource_changes[]? | select(.change.actions != ["no-op"]) | {address: .address, actions: .change.actions}]' plan.json)
      # Module provenance (optional): the configuration's module calls plus the
      # resolved module lockfile, so TSM records which registry modules this state
      # uses and how fresh they are. Both ride the existing jq -n payload via
      # --argjson (never string-concatenated), and are ignored by older servers.
      MODULE_CALLS=$(jq -c '{configuration:{root_module:{module_calls:(.configuration.root_module.module_calls // {})}}}' plan.json)
      MODULE_LOCKS=$( [ -f .terraform/modules/modules.json ] && jq -c . .terraform/modules/modules.json || echo null )
      PAYLOAD=$(jq -n \
        --argjson added "$ADD" --argjson changed "$CHG" --argjson destroyed "$DEL" \
        --argjson drifted "$DRIFTED" --argjson summary "$SUMMARY" \
        --argjson plan "$MODULE_CALLS" --argjson module_locks "$MODULE_LOCKS" \
        --arg detail "azdo build $BUILD_BUILDID" \
        '{status:"completed", added:$added, changed:$changed, destroyed:$destroyed, drifted:$drifted, summary:$summary, plan:$plan, module_locks:$module_locks, detail:$detail}')
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
