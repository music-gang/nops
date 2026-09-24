# A service with a pre-hook that runs a database migration before the new
# version starts (see api-migrate.nomad.hcl). This example is a hook-contract
# reference, not one of the docs/acceptance/ checklists: it parses, deploys
# and dispatches the hook, but the placeholder image below has no
# "/app/migrate" binary, so the hook itself only succeeds once you point
# var.image (api.vars.hcl) at a real image that has one.

job "api" {
  datacenters = ["dc1"]
  type        = "service"

  meta {
    nops_managed          = "true"
    nops_policy           = "approval"
    nops_pre_hook         = "api-migrate"
    nops_pre_hook_timeout = "5m"
  }

  group "api" {
    count = 1

    network {
      port "http" {
        to = 8080
      }
    }

    service {
      name = "api"
      port = "http"
    }

    task "api" {
      driver = "docker"

      config {
        image = var.image
        ports = ["http"]
      }

      resources {
        cpu    = 100
        memory = 128
      }
    }
  }
}

variable "image" {
  type    = string
  default = "traefik/whoami:v1.10"
}
