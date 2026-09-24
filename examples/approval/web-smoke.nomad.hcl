# Post-hook: HTTP smoke test after deploying "web".
#
# In the "web" job:
#   meta {
#     nops_managed   = "true"
#     nops_policy    = "approval"
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

      # Where "web" answers comes from Nomad's own service discovery (the job
      # registers it with provider = "nomad"), so the hook needs nothing but
      # Nomad. It reaches the service on its host address and port.
      template {
        destination = "local/url"
        data        = <<-EOT
          {{ with nomadService "web" }}{{ with index . 0 }}http://{{ .Address }}:{{ .Port }}/{{ end }}{{ end }}
        EOT
      }

      config {
        image   = "curlimages/curl:8.10.1"
        command = "sh"
        args    = ["-c", "curl -fsS --retry 5 --retry-connrefused \"$(cat /local/url)\""]
      }

      resources {
        cpu    = 50
        memory = 64
      }
    }
  }
}
