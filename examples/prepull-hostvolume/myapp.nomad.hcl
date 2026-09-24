# A service with a single-writer host volume, deployed with policy
# "approval" and a pre-hook that pre-pulls the new image on the volume's
# node (see myapp-prepull.nomad.hcl). Client config needed on that node:
#
#   client {
#     host_volume "myapp_data" {
#       path      = "/opt/nomad/volumes/myapp_data"
#       read_only = false
#     }
#   }
#
# Bump var.image (myapp.vars.hcl) to trigger a deployment.

job "myapp" {
  datacenters = [var.datacenter]
  type        = "service"

  meta {
    nops_managed          = "true"
    nops_policy           = "approval"
    nops_pre_hook         = "myapp-prepull"
    nops_pre_hook_timeout = "15m"
  }

  group "myapp" {
    count = 1

    # Must match myapp-prepull.nomad.hcl's volume source: a hook receives no
    # variables (see docs/meta-keys.md#syntax-and-parsing), so it cannot read
    # this one back.
    volume "myapp_data" {
      type   = "host"
      source = "myapp_data"
    }

    network {
      port "http" {
        to = 8080
      }
    }

    service {
      name     = "myapp"
      port     = "http"
      provider = "nomad"
    }

    task "myapp" {
      driver = "docker"

      config {
        image      = var.image
        force_pull = false
        ports      = ["http"]
      }

      volume_mount {
        volume      = "myapp_data"
        destination = "/data"
      }

      resources {
        cpu    = 200
        memory = 256
      }
    }
  }
}

variable "datacenter" {
  type    = string
  default = "dc1"
}

variable "image" {
  type    = string
  default = "nginx:1.27"
}
