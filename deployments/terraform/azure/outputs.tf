# Each output names the values-aks.yaml placeholder it fills.
output "acr_login_server" {
  description = "values-aks.yaml: backend/frontend image repository prefix (<ACR_NAME>.azurecr.io)"
  value       = azurerm_container_registry.main.login_server
}

output "postgres_fqdn" {
  description = "values-aks.yaml: externalDatabase.host"
  value       = azurerm_postgresql_flexible_server.main.fqdn
}

output "key_vault_name" {
  description = "values-aks.yaml: keyVault.name"
  value       = azurerm_key_vault.main.name
}

output "tenant_id" {
  description = "values-aks.yaml: keyVault.tenantId"
  value       = data.azurerm_client_config.current.tenant_id
}

output "app_identity_client_id" {
  description = "values-aks.yaml: workloadIdentity.clientId + keyVault.clientId"
  value       = azurerm_user_assigned_identity.tsm.client_id
}

output "agfc_id" {
  description = "values-aks.yaml: gatewayAPI.albId"
  value       = azurerm_application_load_balancer.main.id
}

output "agfc_frontend_fqdn" {
  description = "Point your DNS CNAME/A record here"
  value       = azurerm_application_load_balancer_frontend.main.fully_qualified_domain_name
}

output "alb_controller_client_id" {
  description = "ALB controller helm install: --set albController.podIdentity.clientID"
  value       = azurerm_user_assigned_identity.alb_controller.client_id
}

output "aks_oidc_issuer" {
  description = "AKS OIDC issuer URL (already bound into the federated credentials)"
  value       = azurerm_kubernetes_cluster.main.oidc_issuer_url
}

output "kubeconfig_command" {
  value = "az aks get-credentials -g ${azurerm_resource_group.main.name} -n ${azurerm_kubernetes_cluster.main.name}"
}
