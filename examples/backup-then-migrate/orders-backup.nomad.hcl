# First pre-hook of "orders": a dump of the database before it is migrated.
#
# In the "orders" job:
#   meta {
#     nops_pre_hook = "orders-backup,orders-migrate"
#   }
#
# How long a hook may run is decided here, on the hook, with nops_timeout (a
# large database takes a while to dump), so every job that uses it gets the
# same limit. The dump goes to a host volume, as in examples/backup-stateful:
#
#   client {
#     host_volume "db_backup" {
#       path      = "/opt/nomad/volumes/db_backup"
#       read_only = false
#     }
#   }

job "orders-backup" {
  datacenters = ["dc1"]
  type        = "batch"

  meta {
    nops_role    = "hook"
    nops_timeout = "30m"
  }

  parameterized {
    meta_required = ["nops_deployment_id"]
  }

  group "backup" {
    volume "db_backup" {
      type   = "host"
      source = "db_backup"
    }

    restart {
      attempts = 0
      mode     = "fail"
    }

    reschedule {
      attempts  = 0
      unlimited = false
    }

    task "dump" {
      driver = "docker"

      template {
        destination = "local/db.env"
        env         = true
        data        = <<-EOT
          {{ with nomadService "db" }}{{ with index . 0 }}PGHOST={{ .Address }}
          PGPORT={{ .Port }}{{ end }}{{ end }}
        EOT
      }

      config {
        image   = "postgres:16-alpine"
        command = "sh"
        args    = ["-c", "pg_dump -U postgres -Fc -f /backup/${NOMAD_META_nops_deployment_id}.dump db"]
      }

      env {
        PGPASSWORD = "acceptance"
      }

      volume_mount {
        volume      = "db_backup"
        destination = "/backup"
      }

      resources {
        cpu    = 100
        memory = 128
      }
    }
  }
}
