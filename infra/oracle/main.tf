terraform {
  required_version = ">= 1.5"

  required_providers {
    oci = {
      source  = "oracle/oci"
      version = "~> 8.27"
    }
  }
}

provider "oci" {
  auth                = "ApiKey"
  config_file_profile = "DEFAULT"
  region              = var.region
}

locals {
  compartment_id = var.compartment_ocid != "" ? var.compartment_ocid : var.tenancy_ocid
  name           = "evsolar"
  is_flex_shape  = var.shape_ocpus > 0
}

# ---------------------------------------------------------------------------
# Network
# ---------------------------------------------------------------------------

resource "oci_core_vcn" "main" {
  compartment_id = local.compartment_id
  display_name   = "${local.name}-vcn"
  cidr_blocks    = ["10.0.0.0/16"]
  dns_label      = "evsolar"
}

resource "oci_core_internet_gateway" "main" {
  compartment_id = local.compartment_id
  vcn_id         = oci_core_vcn.main.id
  display_name   = "${local.name}-igw"
  enabled        = true
}

resource "oci_core_route_table" "public" {
  compartment_id = local.compartment_id
  vcn_id         = oci_core_vcn.main.id
  display_name   = "${local.name}-rt-public"

  route_rules {
    destination       = "0.0.0.0/0"
    destination_type  = "CIDR_BLOCK"
    network_entity_id = oci_core_internet_gateway.main.id
  }
}

resource "oci_core_security_list" "public" {
  compartment_id = local.compartment_id
  vcn_id         = oci_core_vcn.main.id
  display_name   = "${local.name}-sl-public"

  egress_security_rules {
    destination = "0.0.0.0/0"
    protocol    = "all"
  }

  ingress_security_rules {
    protocol    = "6" # TCP
    source      = var.ssh_allowed_cidr
    description = "SSH"
    tcp_options {
      min = 22
      max = 22
    }
  }

  # Vehicles connect from Tesla's LTE carriers and from home Wi-Fi, so the source cannot be
  # narrowed. mTLS is the access control here: fleet-telemetry rejects any peer whose client
  # certificate does not chain to Tesla's CA.
  ingress_security_rules {
    protocol    = "6"
    source      = "0.0.0.0/0"
    description = "Tesla Fleet Telemetry (mTLS WebSocket)"
    tcp_options {
      min = var.telemetry_port
      max = var.telemetry_port
    }
  }

  # Tesla fetches the application's public key from here before it will pair a virtual key or
  # accept a telemetry configuration.
  ingress_security_rules {
    protocol    = "6"
    source      = "0.0.0.0/0"
    description = "Tesla third-party public key over HTTPS"
    tcp_options {
      min = var.public_key_port
      max = var.public_key_port
    }
  }

  # Path MTU discovery. Without this, large TLS handshake records can black-hole.
  ingress_security_rules {
    protocol    = "1" # ICMP
    source      = "0.0.0.0/0"
    description = "Fragmentation needed"
    icmp_options {
      type = 3
      code = 4
    }
  }
}

resource "oci_core_subnet" "public" {
  compartment_id             = local.compartment_id
  vcn_id                     = oci_core_vcn.main.id
  display_name               = "${local.name}-subnet-public"
  cidr_block                 = "10.0.1.0/24"
  route_table_id             = oci_core_route_table.public.id
  security_list_ids          = [oci_core_security_list.public.id]
  dns_label                  = "public"
  prohibit_public_ip_on_vnic = false
}

# ---------------------------------------------------------------------------
# Instance
# ---------------------------------------------------------------------------

# Filtering by shape resolves the architecture automatically: x86_64 images for E2.1.Micro,
# aarch64 images for A1.Flex.
data "oci_core_images" "ubuntu" {
  compartment_id           = local.compartment_id
  operating_system         = "Canonical Ubuntu"
  operating_system_version = "24.04"
  shape                    = var.shape
  sort_by                  = "TIMECREATED"
  sort_order               = "DESC"
}

resource "oci_core_instance" "telemetry" {
  compartment_id      = local.compartment_id
  availability_domain = var.availability_domain
  display_name        = "${local.name}-telemetry"
  shape               = var.shape

  dynamic "shape_config" {
    for_each = local.is_flex_shape ? [1] : []
    content {
      ocpus         = var.shape_ocpus
      memory_in_gbs = var.shape_memory_gb
    }
  }

  source_details {
    source_type = "image"
    source_id   = data.oci_core_images.ubuntu.images[0].id
  }

  create_vnic_details {
    subnet_id        = oci_core_subnet.public.id
    assign_public_ip = true
    hostname_label   = "telemetry"
  }

  metadata = {
    ssh_authorized_keys = var.ssh_public_key
    user_data = base64encode(templatefile("${path.module}/cloud-init.yaml", {
      telemetry_port  = var.telemetry_port
      public_key_port = var.public_key_port
    }))
  }

  # The public IP is ephemeral, which is what the Always Free VNIC includes. It survives reboots
  # and stops/starts, but destroying and recreating the instance issues a new one — at which
  # point the DuckDNS A record has to be updated to match.
  preserve_boot_volume = false

  lifecycle {
    # This instance is only obtainable when Oracle happens to have spare Always Free A1 capacity,
    # which took 71 launch attempts the first time. Terraform must never destroy it to satisfy a
    # config change.
    prevent_destroy = true

    # Both of these force replacement when they change, and neither is worth an instance for:
    # cloud-init only runs on first boot, so editing it cannot affect a live host anyway, and
    # source_id moves every time Canonical publishes a new image.
    ignore_changes = [
      metadata["user_data"],
      source_details[0].source_id,
    ]
  }
}
