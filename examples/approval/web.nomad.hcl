# A stateless service, deployed with policy "approval" and a post-hook that
# smoke-tests it once healthy (see web-smoke.nomad.hcl). No host volume, so
# it needs nothing set up on the client beyond the docker driver.
#
# Bump var.image or var.count (web.vars.hcl) to trigger a deployment: approve,
# reject, supersede with a new commit, and an outside edit (drift) are all
# worth trying on it.

job "web" {
  datacenters = ["dc1"]
  type        = "service"

  meta {
    nops_managed   = "true"
    nops_policy    = "approval"
    nops_post_hook = "web-smoke"
  }

  group "web" {
    count = var.count

    network {
      port "http" {
        to = 8080
      }
    }

    service {
      name     = "web"
      port     = "http"
      provider = "nomad"

      check {
        type     = "http"
        path     = "/"
        interval = "10s"
        timeout  = "2s"
      }
    }

    task "web" {
      driver = "docker"

      config {
        image = var.image
        ports = ["http"]
        args  = ["-port", "8080"]
      }

      resources {
        cpu    = 100
        memory = 128
      }
    }
  }
}

variable "image" {
  type    = string
  default = "traefik/whoami:v1.10"
}

variable "count" {
  type    = number
  default = 1
}
