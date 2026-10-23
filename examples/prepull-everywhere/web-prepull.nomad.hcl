# Pre-hook: pre-pull of the new image on every node, for a service that Nomad
# can place anywhere.
#
# In the "web" job:
#   meta {
#     nops_managed  = "true"
#     nops_policy   = "approval"
#     nops_pre_hook = "web-prepull"
#   }
#   # and in the task: force_pull = false, so the start uses the node's cache.
#
# A sysbatch job runs once on every eligible node of its datacenters, node
# pool and constraints: give it the same as the "web" job, or it pulls where
# the service can't land and skips nodes where it can. A node that fails to
# pull fails the hook, and so the deployment.
#
# Docker's image cache is per node. Each client collects an unused image
# image_delay (3m by default) after its pull ends: raise it on the clients if
# pulls are slow on a large cluster.
#
# entrypoint (not command) fully replaces the image's own ENTRYPOINT, which
# might otherwise do real setup and get OOM-killed with little memory.
#
# Nomad refuses a reschedule block on a sysbatch job: only restart applies.

job "web-prepull" {
  datacenters = ["dc1"]
  type        = "sysbatch"

  meta {
    nops_role    = "hook"
    nops_timeout = "15m"
  }

  parameterized {
    meta_required = ["nops_deployment_id", "nops_image_web"]
  }

  group "prepull" {
    restart {
      attempts = 0
      mode     = "fail"
    }

    task "pull" {
      driver = "docker"

      config {
        image      = "${NOMAD_META_nops_image_web}"
        entrypoint = ["true"]
      }

      resources {
        cpu    = 50
        memory = 64
      }
    }
  }
}
