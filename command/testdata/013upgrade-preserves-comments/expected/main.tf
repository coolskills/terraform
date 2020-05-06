resource foo_instance a {}
resource bar_instance b {}

terraform {
  # Provider requirements go here
  required_providers {
    # Pin bar to this version
    bar = {
      source  = "registry.terraform.io/hashicorp/bar"
      version = "0.5.0"
    }
    foo = {
      source = "registry.terraform.io/hashicorp/foo"
    }
  }
}
