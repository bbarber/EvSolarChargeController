Drop `com.tesla.3p.public-key.pem` here — the **public** half of the EC key pair only.

    openssl ecparam -name prime256v1 -genkey -noout -out fleet-key.pem   # private, keep in .secrets/
    openssl ec -in fleet-key.pem -pubout -out com.tesla.3p.public-key.pem

See `docs/SETUP.md` step 2.
