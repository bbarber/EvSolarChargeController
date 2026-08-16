output "public_ip" {
  description = "Point the DuckDNS telemetry A record at this."
  value       = oci_core_instance.telemetry.public_ip
}

output "ssh_command" {
  value = "ssh -i ~/.ssh/oci_evsolar ubuntu@${oci_core_instance.telemetry.public_ip}"
}

output "shape_launched" {
  description = "Confirms which architecture won, since that decides whether images need an arm64 rebuild."
  value       = oci_core_instance.telemetry.shape
}

output "image_used" {
  value = data.oci_core_images.ubuntu.images[0].display_name
}
