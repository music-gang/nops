# Pre-hook: pre-pull of a heavy image for a service that uses a host volume.
#
# In the "myapp" job (a single write mount, so no canary):
#   meta {
#     nops_managed          = "true"
#     nops_policy           = "approval"
#     nops_pre_hook         = "myapp-prepull"
#     nops_pre_hook_timeout = "15m"
#   }
#   # and in the task: force_pull = false, so the start uses the node's cache.
#
# Docker's image cache is per node: any task that pulls image:tag on that node
# warms it for the real deploy. The "myapp" job has no explicit constraint: it
# is pinned to whichever node has the "myapp_data" host volume. Mounting the
# same volume (read-only) here places the hook on the same node without
# hardcoding a node ID.
#
# entrypoint (not command) fully replaces the image's own ENTRYPOINT, which
# might otherwise do real setup and get OOM-killed with little memory.

job "myapp-prepull" {
  datacenters = ["dc1"]
  type        = "batch"

  meta {
    nops_role = "hook"
  }

  parameterized {
    meta_required = ["nops_deployment_id", "nops_image_myapp"]
  }

  group "prepull" {
    volume "myapp_data" {
      type            = "host"
      source          = "myapp_data"
      access_mode     = "single-node-writer"
      attachment_mode = "file-system"
    }

    restart {
      attempts = 0
      mode     = "fail"
    }

    reschedule {
      attempts  = 0
      unlimited = false
    }

    task "pull" {
      driver = "docker"

      config {
        image      = "${NOMAD_META_nops_image_myapp}"
        entrypoint = ["true"]
      }

      volume_mount {
        volume      = "myapp_data"
        destination = "/data"
        read_only   = true
      }

      resources {
        cpu    = 50
        memory = 64
      }
    }
  }
}
