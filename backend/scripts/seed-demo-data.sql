-- Seed demo data for Terraform State Manager
-- This script inserts deterministic demo data for testing and development.
-- It uses fixed UUIDs so the script is idempotent when run against an empty database.
--
-- Prerequisites: run all migrations first (000001 through 000010).

BEGIN;

-- ============================================================
-- 1. Demo Organization
-- ============================================================
INSERT INTO organizations (id, name, display_name, description, is_active)
VALUES (
    '00000000-0000-0000-0000-000000000001',
    'demo-org',
    'Demo Org',
    'Demo organization for testing and evaluation',
    true
) ON CONFLICT (id) DO NOTHING;

-- ============================================================
-- 2. Demo Admin User
-- ============================================================
INSERT INTO users (id, email, name, is_active)
VALUES (
    '00000000-0000-0000-0000-000000000002',
    'admin@demo.local',
    'Demo Admin',
    true
) ON CONFLICT (id) DO NOTHING;

-- ============================================================
-- 3. Role Template Assignment (admin role)
-- ============================================================
-- Look up the system admin role template and assign the demo user to Demo Org.
INSERT INTO organization_members (id, organization_id, user_id, role_template_id)
SELECT
    '00000000-0000-0000-0000-000000000003',
    '00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-000000000002',
    rt.id
FROM role_templates rt
WHERE rt.name = 'admin'
ON CONFLICT (organization_id, user_id) DO NOTHING;

-- ============================================================
-- 4. State Source (HCP Terraform type)
-- ============================================================
INSERT INTO state_sources (id, organization_id, name, source_type, config, is_active, created_by)
VALUES (
    '00000000-0000-0000-0000-000000000004',
    '00000000-0000-0000-0000-000000000001',
    'demo-hcp-terraform',
    'hcp_terraform',
    '{
        "organization": "demo-tf-org",
        "token_encrypted": "placeholder-encrypted-token",
        "base_url": "https://app.terraform.io"
    }'::jsonb,
    true,
    '00000000-0000-0000-0000-000000000002'
) ON CONFLICT (organization_id, name) DO NOTHING;

-- ============================================================
-- 5. Analysis Run with Sample Results
-- ============================================================
INSERT INTO analysis_runs (
    id, organization_id, source_id, status, trigger_type,
    started_at, completed_at,
    total_workspaces, successful_count, failed_count,
    total_rum, total_managed, total_resources, total_data_sources,
    performance_ms, triggered_by
) VALUES (
    '00000000-0000-0000-0000-000000000005',
    '00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-000000000004',
    'completed',
    'manual',
    NOW() - INTERVAL '10 minutes',
    NOW() - INTERVAL '8 minutes',
    3, 3, 0,
    42, 150, 192, 12,
    120000,
    '00000000-0000-0000-0000-000000000002'
) ON CONFLICT (id) DO NOTHING;

-- Analysis result: workspace-production
INSERT INTO analysis_results (
    id, run_id, workspace_name, organization, status,
    total_resources, managed_count, rum_count, data_source_count,
    resources_by_type, provider_analysis, terraform_version, analysis_method
) VALUES (
    '00000000-0000-0000-0000-000000000006',
    '00000000-0000-0000-0000-000000000005',
    'workspace-production',
    'demo-tf-org',
    'success',
    80, 65, 18, 5,
    '{"aws_instance": 10, "aws_s3_bucket": 8, "aws_iam_role": 12, "aws_lambda_function": 5, "aws_rds_instance": 3}'::jsonb,
    '{"aws": {"resource_count": 65, "version": "~> 5.0"}}'::jsonb,
    '1.9.0',
    'api'
) ON CONFLICT (id) DO NOTHING;

-- Analysis result: workspace-staging
INSERT INTO analysis_results (
    id, run_id, workspace_name, organization, status,
    total_resources, managed_count, rum_count, data_source_count,
    resources_by_type, provider_analysis, terraform_version, analysis_method
) VALUES (
    '00000000-0000-0000-0000-000000000007',
    '00000000-0000-0000-0000-000000000005',
    'workspace-staging',
    'demo-tf-org',
    'success',
    62, 50, 14, 4,
    '{"aws_instance": 6, "aws_s3_bucket": 5, "aws_iam_role": 8, "aws_lambda_function": 3, "aws_rds_instance": 2}'::jsonb,
    '{"aws": {"resource_count": 50, "version": "~> 5.0"}}'::jsonb,
    '1.9.0',
    'api'
) ON CONFLICT (id) DO NOTHING;

-- Analysis result: workspace-dev
INSERT INTO analysis_results (
    id, run_id, workspace_name, organization, status,
    total_resources, managed_count, rum_count, data_source_count,
    resources_by_type, provider_analysis, terraform_version, analysis_method
) VALUES (
    '00000000-0000-0000-0000-000000000008',
    '00000000-0000-0000-0000-000000000005',
    'workspace-dev',
    'demo-tf-org',
    'success',
    50, 35, 10, 3,
    '{"aws_instance": 4, "aws_s3_bucket": 3, "aws_iam_role": 6, "aws_lambda_function": 2, "aws_rds_instance": 1}'::jsonb,
    '{"aws": {"resource_count": 35, "version": "~> 5.0"}}'::jsonb,
    '1.8.5',
    'api'
) ON CONFLICT (id) DO NOTHING;

-- ============================================================
-- 6. Alert Rules
-- ============================================================
INSERT INTO alert_rules (id, organization_id, name, rule_type, config, severity, is_active)
VALUES (
    '00000000-0000-0000-0000-000000000009',
    '00000000-0000-0000-0000-000000000001',
    'High RUM count',
    'rum_threshold',
    '{"threshold": 100, "comparison": "greater_than"}'::jsonb,
    'warning',
    true
) ON CONFLICT (id) DO NOTHING;

INSERT INTO alert_rules (id, organization_id, name, rule_type, config, severity, is_active)
VALUES (
    '00000000-0000-0000-0000-00000000000a',
    '00000000-0000-0000-0000-000000000001',
    'Outdated Terraform version',
    'version_check',
    '{"min_version": "1.9.0"}'::jsonb,
    'info',
    true
) ON CONFLICT (id) DO NOTHING;

-- ============================================================
-- 7. Sample Alerts
-- ============================================================
INSERT INTO alerts (id, organization_id, rule_id, workspace_name, severity, title, description, metadata, is_acknowledged)
VALUES (
    '00000000-0000-0000-0000-00000000000b',
    '00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-000000000009',
    'workspace-production',
    'warning',
    'RUM count exceeds warning threshold',
    'workspace-production has 18 resources under management which is approaching the threshold of 100.',
    '{"current_value": 18, "threshold": 100}'::jsonb,
    false
) ON CONFLICT (id) DO NOTHING;

INSERT INTO alerts (id, organization_id, rule_id, workspace_name, severity, title, description, metadata, is_acknowledged)
VALUES (
    '00000000-0000-0000-0000-00000000000c',
    '00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-00000000000a',
    'workspace-dev',
    'info',
    'Terraform version outdated',
    'workspace-dev is running Terraform 1.8.5 which is below the recommended minimum of 1.9.0.',
    '{"current_version": "1.8.5", "min_version": "1.9.0"}'::jsonb,
    false
) ON CONFLICT (id) DO NOTHING;

INSERT INTO alerts (id, organization_id, rule_id, workspace_name, severity, title, description, metadata, is_acknowledged, acknowledged_by, acknowledged_at)
VALUES (
    '00000000-0000-0000-0000-00000000000d',
    '00000000-0000-0000-0000-000000000001',
    '00000000-0000-0000-0000-00000000000a',
    'workspace-staging',
    'info',
    'Terraform version check passed',
    'workspace-staging is running Terraform 1.9.0 which meets the minimum version requirement.',
    '{"current_version": "1.9.0", "min_version": "1.9.0"}'::jsonb,
    true,
    '00000000-0000-0000-0000-000000000002',
    NOW() - INTERVAL '1 hour'
) ON CONFLICT (id) DO NOTHING;

COMMIT;
