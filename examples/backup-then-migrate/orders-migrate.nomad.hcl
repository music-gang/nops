# Second pre-hook of "orders": the migration, once the backup has succeeded.
#
# It uses the same image as the new version (nops_image_orders). The command
# must be idempotent: nops may dispatch the hook again after one of its own
# crashes. Its timeout is its own: a migration is quicker than a dump.

job "orders-migrate" {
  datacenters = ["dc1"]
  type        = "batch"

  meta {
    nops_role    = "hook"
    nops_timeout = "5m"
  }

  parameterized {
    meta_required = ["nops_deployment_id", "nops_image_orders"]
  }

  group "migrate" {
    restart {
      attempts = 0
      mode     = "fail"
    }

    reschedule {
      attempts  = 0
      unlimited = false
    }

    task "migrate" {
      driver = "docker"

      config {
        image   = "${NOMAD_META_nops_image_orders}"
        command = "/app/migrate"
        args    = ["up"]
      }

      resources {
        cpu    = 100
        memory = 128
      }
    }
  }
}
