# certbot plus the DuckDNS DNS-01 plugin, pinned so a weekly renewal never meets a surprise
# upgrade. Built on the VM by renew-cert.sh; the build is cached after the first run.
FROM python:3.12-slim
RUN pip install --no-cache-dir --root-user-action=ignore \
      certbot==5.10.0 certbot-dns-duckdns==1.9.0
ENTRYPOINT ["certbot"]
