# Post-hook: HTTP smoke test after deploying "web".
#
# In the "web" job:
#   meta {
#     nops_managed   = "true"
#     nops_policy    = "auto"
#     nops_post_hook = "web-smoke"
#   }
#
# It runs once the new version is healthy. If it fails, the deployment goes to
# failed with a notification (there is no automatic rollback).

job "web-smoke" {
  datacenters = ["dc1"]
  type        = "batch"

  meta {
    nops_role = "hook"
  }

  parameterized {
    meta_required = ["nops_deployment_id"]
  }

  group "smoke" {
    restart {
      attempts = 0
      mode     = "fail"
    }

    reschedule {
      attempts  = 0
      unlimited = false
    }

    task "check" {
      driver = "docker"

      config {
        image   = "curlimages/curl:8.10.1"
        command = "curl"
        args    = ["-fsS", "--retry", "5", "--retry-connrefused", "http://web.service.consul:8080/healthz"]
      }

      resources {
        cpu    = 50
        memory = 64
      }
    }
  }
}
