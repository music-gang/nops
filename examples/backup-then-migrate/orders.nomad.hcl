# A service with two pre-hooks, backup then migrate: nops runs them one after
# the other, in the order they are listed, once the deployment is approved, and
# only then registers the new version. If the backup fails, the migration never
# runs; if either fails, the live job is left as it is.
#
# The database is the "db" job of examples/backup-stateful (it registers itself
# as "db" with Nomad's service discovery, which the backup reads). The image
# below is a placeholder without a "/app/migrate" binary: point var.image
# (orders.vars.hcl) at a real one for the migration to succeed.

job "orders" {
  datacenters = ["dc1"]
  type        = "service"

  meta {
    nops_managed  = "true"
    nops_policy   = "approval"
    nops_pre_hook = "orders-backup,orders-migrate"
  }

  group "orders" {
    count = 1

    network {
      port "http" {
        to = 8080
      }
    }

    service {
      name     = "orders"
      port     = "http"
      provider = "nomad"
    }

    task "orders" {
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
