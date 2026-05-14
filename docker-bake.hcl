# Documentation for bake file
# https://docs.docker.com/build/bake/reference/
#

variable "REPO" {
    default = "docker.io/weverkley"
}

variable "TAG" {
  default = "latest"
}

variable "PG_VERSION" {
    default = "16"
}

variable "GOLANG_VERSION" {
    default = "1.26-alpine"
}

variable "UBUNTU_VERSION" {
    default = "24.04"
}

variable "BUCARDO_VERSION" {
    default = "5.6.0"
}

target "default" {
    dockerfile = "Dockerfile"
    context = "."
    pull = true
    args = {
      PG_VERSION = PG_VERSION      
      GOLANG_VERSION = GOLANG_VERSION
      UBUNTU_VERSION = UBUNTU_VERSION
      BUCARDO_VERSION = BUCARDO_VERSION
    }    
    tags = ["${REPO}/bucardo:${TAG}"]
    platforms = ["linux/amd64"]
}
