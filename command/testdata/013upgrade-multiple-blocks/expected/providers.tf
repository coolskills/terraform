terraform {
  required_providers {
    bar = {
      source = "registry.acme.corp/acme/bar"
    }
    baz = {
      source  = "registry.terraform.io/terraform-providers/baz"
      version = "~> 2.0.0"
    }
    foo = {
      source  = "registry.terraform.io/hashicorp/foo"
      version = "0.5"
    }
  }
}
