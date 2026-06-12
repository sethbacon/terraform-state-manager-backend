// ============================================================
// Azure Container Apps deployment for Terraform State Manager
// ============================================================
// Three apps: backend (API), worker (single replica — schedule runner +
// state sync), frontend (nginx SPA, externally exposed).
//
// IMPORTANT: the backend app is named literally "backend" and exposes port
// 8080 via additionalPortMappings. The frontend image's nginx proxies to
// http://backend:8080 (compose-era service name); ACA name-based resolution
// plus the extra port mapping makes that work unmodified.
//
// Deploy:
//   az deployment group create -g <rg> -f main.bicep -p parameters.json
// See docs/deployment/azure-container-apps.md.

@description('Azure region for all resources')
param location string = resourceGroup().location

@description('Name prefix for environment-level resources')
param environmentName string = 'tsm'

@description('Backend image, e.g. myacr.azurecr.io/terraform-state-manager-backend:v1.0.0')
param backendImage string

@description('Frontend image, e.g. myacr.azurecr.io/terraform-state-manager-frontend:v1.0.0')
param frontendImage string

@description('PostgreSQL server FQDN (Azure Database for PostgreSQL Flexible Server)')
param databaseHost string

@description('PostgreSQL database name')
param databaseName string = 'terraform_state_manager'

@description('PostgreSQL user')
param databaseUser string = 'tsm'

@secure()
@description('PostgreSQL password')
param databasePassword string

@secure()
@description('JWT signing secret (min 32 chars; openssl rand -hex 32)')
param jwtSecret string

@secure()
@description('Encryption key (32 raw bytes or 64 hex chars). Escrow it — losing it orphans stored credentials.')
param encryptionKey string

@description('Public custom domain (empty = use the frontend default FQDN)')
param customDomain string = ''

@description('Backend min replicas')
@minValue(1)
param backendMinReplicas int = 1

@description('Backend max replicas')
param backendMaxReplicas int = 10

@description('ACR login server (empty when images are public)')
param acrLoginServer string = ''

@description('ACR username (when not using managed identity)')
param acrUsername string = ''

@secure()
@description('ACR password (when not using managed identity)')
param acrPassword string = ''

@description('Enable Entra ID (OIDC) login')
param oidcEnabled bool = false

@description('OIDC issuer URL, e.g. https://login.microsoftonline.com/<tenant>/v2.0')
param oidcIssuerUrl string = ''

@description('OIDC client (app registration) ID')
param oidcClientId string = ''

@secure()
@description('OIDC client secret')
param oidcClientSecret string = ''

var publicUrl = !empty(customDomain)
  ? 'https://${customDomain}'
  : 'https://${environmentName}-frontend.${containerAppEnv.properties.defaultDomain}'

var registries = !empty(acrLoginServer) ? [
  {
    server: acrLoginServer
    username: acrUsername
    passwordSecretRef: 'acr-password'
  }
] : []

var acrSecret = !empty(acrLoginServer) ? [
  { name: 'acr-password', value: acrPassword }
] : []

var commonSecrets = concat([
  { name: 'database-password', value: databasePassword }
  { name: 'jwt-secret', value: jwtSecret }
  { name: 'encryption-key', value: encryptionKey }
], oidcEnabled ? [
  { name: 'oidc-client-secret', value: oidcClientSecret }
] : [], acrSecret)

var commonEnv = concat([
  { name: 'TSM_SERVER_HOST', value: '0.0.0.0' }
  { name: 'TSM_SERVER_PORT', value: '8080' }
  { name: 'TSM_SERVER_BASE_URL', value: publicUrl }
  { name: 'TSM_SERVER_PUBLIC_URL', value: publicUrl }
  // CI runners post drift/version-lab results here (via the frontend proxy).
  { name: 'TSM_SERVER_CALLBACK_URL', value: publicUrl }
  { name: 'TSM_DATABASE_HOST', value: databaseHost }
  { name: 'TSM_DATABASE_PORT', value: '5432' }
  { name: 'TSM_DATABASE_NAME', value: databaseName }
  { name: 'TSM_DATABASE_USER', value: databaseUser }
  { name: 'TSM_DATABASE_PASSWORD', secretRef: 'database-password' }
  { name: 'TSM_DATABASE_SSL_MODE', value: 'require' }
  { name: 'TSM_JWT_SECRET', secretRef: 'jwt-secret' }
  { name: 'TSM_ENCRYPTION_KEY', secretRef: 'encryption-key' }
  { name: 'TSM_LOGGING_LEVEL', value: 'info' }
  { name: 'TSM_LOGGING_FORMAT', value: 'json' }
  { name: 'TSM_TELEMETRY_METRICS_ENABLED', value: 'true' }
  { name: 'TSM_TELEMETRY_METRICS_PROMETHEUS_PORT', value: '9090' }
  { name: 'DEV_MODE', value: 'false' }
  { name: 'TSM_AUTH_OIDC_ENABLED', value: string(oidcEnabled) }
], oidcEnabled ? [
  { name: 'TSM_AUTH_OIDC_ISSUER_URL', value: oidcIssuerUrl }
  { name: 'TSM_AUTH_OIDC_CLIENT_ID', value: oidcClientId }
  { name: 'TSM_AUTH_OIDC_CLIENT_SECRET', secretRef: 'oidc-client-secret' }
  { name: 'TSM_AUTH_OIDC_REDIRECT_URL', value: '${publicUrl}/api/v1/auth/callback' }
] : [])

var backendProbes = [
  {
    type: 'Startup'
    httpGet: { path: '/health', port: 8080 }
    initialDelaySeconds: 5
    periodSeconds: 5
    failureThreshold: 12
  }
  {
    type: 'Liveness'
    httpGet: { path: '/health', port: 8080 }
    periodSeconds: 30
  }
  {
    type: 'Readiness'
    httpGet: { path: '/ready', port: 8080 }
    initialDelaySeconds: 5
    periodSeconds: 10
  }
]

// ---------------------------------------------------------------------------
// Log Analytics + Container Apps Environment
// ---------------------------------------------------------------------------
resource logAnalytics 'Microsoft.OperationalInsights/workspaces@2022-10-01' = {
  name: '${environmentName}-logs'
  location: location
  properties: {
    sku: { name: 'PerGB2018' }
    retentionInDays: 30
  }
}

resource containerAppEnv 'Microsoft.App/managedEnvironments@2024-03-01' = {
  name: '${environmentName}-env'
  location: location
  properties: {
    appLogsConfiguration: {
      destination: 'log-analytics'
      logAnalyticsConfiguration: {
        customerId: logAnalytics.properties.customerId
        sharedKey: logAnalytics.listKeys().primarySharedKey
      }
    }
  }
}

// ---------------------------------------------------------------------------
// Backend API — app name MUST stay "backend" (frontend nginx upstream)
// ---------------------------------------------------------------------------
resource backendApp 'Microsoft.App/containerApps@2024-03-01' = {
  name: 'backend'
  location: location
  properties: {
    managedEnvironmentId: containerAppEnv.id
    configuration: {
      activeRevisionsMode: 'Single'
      ingress: {
        external: false
        targetPort: 8080
        transport: 'http'
        // Lets the frontend reach http://backend:8080 directly (the baked
        // nginx upstream) instead of only via the :80/:443 internal ingress.
        additionalPortMappings: [
          {
            external: false
            targetPort: 8080
            exposedPort: 8080
          }
        ]
      }
      secrets: commonSecrets
      registries: registries
    }
    template: {
      containers: [
        {
          name: 'backend'
          image: backendImage
          resources: { cpu: json('0.5'), memory: '1Gi' }
          env: concat(commonEnv, [
            // Periodic workers run in the dedicated worker app below.
            { name: 'TSM_WORKERS_ENABLED', value: 'false' }
          ])
          probes: backendProbes
        }
      ]
      scale: {
        minReplicas: backendMinReplicas
        maxReplicas: backendMaxReplicas
        rules: [
          {
            name: 'http-scaling'
            http: { metadata: { concurrentRequests: '50' } }
          }
        ]
      }
    }
  }
}

// ---------------------------------------------------------------------------
// Worker — schedule runner + state sync. EXACTLY ONE replica (min=max=1):
// the schedule runner has no cross-replica claim.
// ---------------------------------------------------------------------------
resource workerApp 'Microsoft.App/containerApps@2024-03-01' = {
  name: '${environmentName}-worker'
  location: location
  properties: {
    managedEnvironmentId: containerAppEnv.id
    configuration: {
      activeRevisionsMode: 'Single'
      secrets: commonSecrets
      registries: registries
    }
    template: {
      containers: [
        {
          name: 'worker'
          image: backendImage
          resources: { cpu: json('0.5'), memory: '1Gi' }
          env: concat(commonEnv, [
            { name: 'TSM_WORKERS_ENABLED', value: 'true' }
          ])
          probes: backendProbes
        }
      ]
      scale: {
        minReplicas: 1
        maxReplicas: 1
      }
    }
  }
}

// ---------------------------------------------------------------------------
// Frontend — external ingress; nginx serves the SPA and proxies /api, /scim,
// /health, /ready, /swagger to http://backend:8080.
// ---------------------------------------------------------------------------
resource frontendApp 'Microsoft.App/containerApps@2024-03-01' = {
  name: '${environmentName}-frontend'
  location: location
  properties: {
    managedEnvironmentId: containerAppEnv.id
    configuration: {
      activeRevisionsMode: 'Single'
      ingress: {
        external: true
        targetPort: 80
        transport: 'http'
      }
      secrets: acrSecret
      registries: registries
    }
    template: {
      containers: [
        {
          name: 'frontend'
          image: frontendImage
          resources: { cpu: json('0.25'), memory: '0.5Gi' }
          probes: [
            {
              type: 'Startup'
              httpGet: { path: '/', port: 80 }
              initialDelaySeconds: 5
              periodSeconds: 5
              failureThreshold: 6
            }
            {
              type: 'Liveness'
              httpGet: { path: '/', port: 80 }
              periodSeconds: 30
            }
          ]
        }
      ]
      scale: {
        minReplicas: 1
        maxReplicas: 5
      }
    }
  }
}

output frontendUrl string = 'https://${frontendApp.properties.configuration.ingress.fqdn}'
output backendInternalFqdn string = backendApp.properties.configuration.ingress.fqdn
output logAnalyticsWorkspaceId string = logAnalytics.id
