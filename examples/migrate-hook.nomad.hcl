# Pre-hook: database migration before deploying "api".
#
# In the "api" job:
#   meta {
#     nops_managed          = "true"
#     nops_policy           = "approval"
#     nops_pre_hook         = "api-migrate"
#     nops_pre_hook_timeout = "5m"
#   }
#
# It uses the same image as the new version (nops_image_api). The command must
# be idempotent: nops may dispatch the hook again after one of its own crashes.

job "api-migrate" {
  datacenters = ["dc1"]
  type        = "batch"

  meta {
    nops_role = "hook"
  }

  parameterized {
    meta_required = ["nops_deployment_id", "nops_image_api"]
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
        image   = "${NOMAD_META_nops_image_api}"
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
