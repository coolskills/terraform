# This is a file called providers.tf which does not originally have a
# required_providers block. 
resource foo_resource a {}
terraform {
  required_providers {
    bar = {
      source = "registry.terraform.io/hashicorp/bar"
    }
    baz = {
      source = "registry.terraform.io/terraform-providers/baz"
    }
    foo = {
      source = "registry.terraform.io/hashicorp/foo"
    }
  }
}
