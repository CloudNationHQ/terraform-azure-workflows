# Shared tflint config, used by .github/workflows/linting.yml for every module repo.
# Bump plugin versions here, and only to releases that are at least two weeks old.

config {
  call_module_type = "local"
}

plugin "terraform" {
  enabled = true
  preset  = "recommended"
  version = "0.16.1"
  source  = "github.com/terraform-linters/tflint-ruleset-terraform"
}

plugin "azurerm" {
  enabled = true
  version = "0.33.0"
  source  = "github.com/terraform-linters/tflint-ruleset-azurerm"
}

# Lifecycle arguments must be literals, so a module cannot make prevent_destroy
# configurable. Hard-coding it would stop consumers from ever removing the
# resource. Protection belongs in the calling configuration, not in the module.
rule "azurerm_resources_missing_prevent_destroy" {
  enabled = false
}
