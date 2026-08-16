variable "tenancy_ocid" {
  description = "Tenancy OCID. Also used as the root compartment unless compartment_ocid is set."
  type        = string
}

variable "compartment_ocid" {
  description = "Compartment to build in. Defaults to the tenancy root."
  type        = string
  default     = ""
}

variable "region" {
  description = "Home region. Always Free compute only exists in the home region, and it cannot be changed."
  type        = string
  default     = "us-chicago-1"
}

variable "availability_domain" {
  description = <<-EOT
    AD to launch in. The Always Free AMD micro quota lives in AD-2 only for this tenancy
    (vm-standard-e2-1-micro-count is 2 in AD-2 and 0 in AD-1/AD-3), so this is not arbitrary.
  EOT
  type        = string
  default     = "GoOx:US-CHICAGO-1-AD-2"
}

variable "shape" {
  description = <<-EOT
    VM.Standard.E2.1.Micro is the x86 Always Free shape and keeps the existing amd64 container
    images working unchanged. If a launch is rejected because the E2 family is not deployed in
    this region, switch to VM.Standard.A1.Flex and set shape_ocpus/shape_memory_gb below — that
    is aarch64, and every image then needs rebuilding for linux/arm64.
  EOT
  type        = string
  default     = "VM.Standard.E2.1.Micro"
}

variable "shape_ocpus" {
  description = "Only for flexible shapes (A1.Flex). Leave at 0 for E2.1.Micro, which is fixed-size."
  type        = number
  default     = 0
}

variable "shape_memory_gb" {
  description = "Only for flexible shapes (A1.Flex). Always Free allows 2 OCPU / 12 GB as of June 2026."
  type        = number
  default     = 0
}

variable "ssh_public_key" {
  description = "Public key authorised for the ubuntu user."
  type        = string
}

variable "ssh_allowed_cidr" {
  description = <<-EOT
    Source range allowed to reach SSH. Defaults to the whole internet because a home IP moves;
    password auth is disabled in the Ubuntu cloud image, so this is key-only either way.
    Tighten it to a /32 if the address is stable.
  EOT
  type        = string
  default     = "0.0.0.0/0"
}

variable "telemetry_port" {
  description = <<-EOT
    Port vehicles connect to. Registered in fleet_telemetry_config, so changing it means
    re-registering both VINs. Must stay open to the whole internet: each car presents its own
    client certificate from an address we cannot predict.
  EOT
  type        = number
  default     = 8443
}

variable "public_key_port" {
  description = <<-EOT
    Serves /.well-known/appspecific/com.tesla.3p.public-key.pem over HTTPS.

    This has to live on the same host as telemetry: Tesla rejects a fleet_telemetry_config whose
    hostname is not under the domain the partner account is registered with ("hostname domain does
    not match with partner account"), and a DuckDNS name carries a single A record. So one name
    must serve both the key on 443 and telemetry on 8443.
  EOT
  type        = number
  default     = 443
}
