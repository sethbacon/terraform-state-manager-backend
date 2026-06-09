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
          PAYLOAD=$(jq -n \
            --argjson added "$ADD" --argjson changed "$CHG" --argjson destroyed "$DEL" \
            --argjson drifted "$DRIFTED" --argjson summary "$SUMMARY" \
            --arg detail "github run $GITHUB_RUN_ID" \
            '{status:"completed", added:$added, changed:$changed, destroyed:$destroyed, drifted:$drifted, summary:$summary, detail:$detail}')
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
      PAYLOAD=$(jq -n \
        --argjson added "$ADD" --argjson changed "$CHG" --argjson destroyed "$DEL" \
        --argjson drifted "$DRIFTED" --argjson summary "$SUMMARY" \
        --arg detail "azdo build $BUILD_BUILDID" \
        '{status:"completed", added:$added, changed:$changed, destroyed:$destroyed, drifted:$drifted, summary:$summary, detail:$detail}')
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
