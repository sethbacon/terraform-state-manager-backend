terraform {
  required_version = ">= 1.5.0, < 2.0.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0.0"
    }
  }
}

module "network" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "~> 5.0"
}

module "labels" {
  source  = "cloudposse/label/null"
  version = "0.25.0"
}

module "local_helper" {
  source = "./modules/helper"
}
