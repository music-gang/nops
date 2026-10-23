# A service deployed with policy "approval" and a pre-hook that pre-pulls the
# new image on every node (see web-prepull.nomad.hcl), so it starts from the
# node's cache wherever Nomad places it.
#
# Bump var.image (web.vars.hcl) to trigger a deployment.

job "web" {
  datacenters = [var.datacenter]
  type        = "service"

  meta {
    nops_managed  = "true"
    nops_policy   = "approval"
    nops_pre_hook = "web-prepull"
  }

  group "web" {
    count = 3

    network {
      port "http" {
        to = 8080
      }
    }

    service {
      name     = "web"
      port     = "http"
      provider = "nomad"
    }

    task "web" {
      driver = "docker"

      config {
        image      = var.image
        force_pull = false
        ports      = ["http"]
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
