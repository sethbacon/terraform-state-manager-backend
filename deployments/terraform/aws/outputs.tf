output "ecr_backend_url" {
  description = "values-eks.yaml: backend.image.repository"
  value       = aws_ecr_repository.backend.repository_url
}

output "ecr_frontend_url" {
  description = "values-eks.yaml: frontend.image.repository"
  value       = aws_ecr_repository.frontend.repository_url
}

output "rds_endpoint" {
  description = "values-eks.yaml: externalDatabase.host"
  value       = aws_db_instance.main.address
}

output "irsa_role_arn" {
  description = "values-eks.yaml: serviceAccount.annotations eks.amazonaws.com/role-arn"
  value       = aws_iam_role.tsm_irsa.arn
}

output "cluster_name" {
  value = aws_eks_cluster.main.name
}

output "kubeconfig_command" {
  value = "aws eks update-kubeconfig --name ${aws_eks_cluster.main.name} --region ${var.region}"
}
