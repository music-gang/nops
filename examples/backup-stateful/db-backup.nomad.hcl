# Pre-hook: pg_dump backup before deploying "db".
#
# In the "db" job:
#   meta {
#     nops_managed          = "true"
#     nops_policy           = "approval"
#     nops_pre_hook         = "db-backup"
#     nops_pre_hook_timeout = "30m"
#   }
#
# It runs after approval, so the backup is fresh, and before apply, so a bad
# migration can be restored from it. pg_dump connects to the running "db"
# service over the network (found through Nomad's service discovery), not
# the filesystem, so this hook only needs the volume it writes the dump to,
# never "db"'s own data volume.
# The timeout is generous: a large database can take a while to dump.

job "db-backup" {
  datacenters = ["dc1"]
  type        = "batch"

  meta {
    nops_role = "hook"
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

      # PGHOST and PGPORT come from Nomad's own service discovery (the "db"
      # job registers it with provider = "nomad"): pg_dump reads them itself.
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
