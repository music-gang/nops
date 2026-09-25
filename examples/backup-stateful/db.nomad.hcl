# A stateful service (Postgres on a host volume), deployed with policy
# "approval" and a pre-hook that backs it up before the new version starts
# (see db-backup.nomad.hcl). Client config needed on the volume's node:
#
#   client {
#     host_volume "db_data" {
#       path      = "/opt/nomad/volumes/db_data"
#       read_only = false
#     }
#     host_volume "db_backup" {
#       path      = "/opt/nomad/volumes/db_backup"
#       read_only = false
#     }
#   }
#
# The password below is fine for a throwaway run, never for a real database.
# Bump var.image (db.vars.hcl) to trigger a deployment.

job "db" {
  datacenters = ["dc1"]
  type        = "service"

  meta {
    nops_managed  = "true"
    nops_policy   = "approval"
    nops_pre_hook = "db-backup"
  }

  group "db" {
    count = 1

    volume "db_data" {
      type   = "host"
      source = "db_data"
    }

    network {
      port "postgres" {
        static = 5432
      }
    }

    service {
      name     = "db"
      port     = "postgres"
      provider = "nomad"
    }

    task "postgres" {
      driver = "docker"

      config {
        image      = var.image
        force_pull = false
        ports      = ["postgres"]
      }

      volume_mount {
        volume      = "db_data"
        destination = "/var/lib/postgresql/data"
      }

      env {
        POSTGRES_PASSWORD = "acceptance"
        PGDATA             = "/var/lib/postgresql/data/pgdata"
      }

      resources {
        cpu    = 200
        memory = 512
      }
    }
  }
}

variable "image" {
  type    = string
  default = "postgres:16-alpine"
}
