module "dns_delegated" {
  source  = "cloudposse/stack-config/yaml//modules/remote-state"
  version = "1.8.0"

  component   = var.dns_delegated_component_name
  environment = var.dns_delegated_environment_name

  context = module.this.context
}
