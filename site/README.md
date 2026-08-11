# Public key host

Everything in this directory is published to GitHub Pages by
`.github/workflows/deploy-pages.yml`. It exists for exactly one reason: Tesla requires the
application's EC public key to be fetchable at

```
https://<application-domain>/.well-known/appspecific/com.tesla.3p.public-key.pem
```

before a virtual key can be paired with a vehicle.

GitHub Pages is used rather than an Azure resource because it is free, serves a real
Let's Encrypt certificate on a custom domain automatically, and needs no renewal job. The key is
public by definition, so hosting it in a public repository is fine.

## Files

| File | Why |
|---|---|
| `.nojekyll` | Without it, Jekyll strips dot-directories and `.well-known/` never gets published. |
| `CNAME` | The custom domain GitHub Pages serves this site on. |
| `.well-known/appspecific/com.tesla.3p.public-key.pem` | The key itself. |

## Publishing the key

Generate the pair (see `docs/SETUP.md` step 2), then copy **only the public half** here:

```bash
cp .secrets/tesla-keys/public-key.pem \
   site/.well-known/appspecific/com.tesla.3p.public-key.pem
```

`fleet-key.pem` is the private key and must never land in this directory — `.gitignore` allows
`*.pem` through only under `site/.well-known/`, specifically so a stray copy of the private key
cannot be committed by accident.

## Verifying

```bash
curl -sI https://<your-domain>/.well-known/appspecific/com.tesla.3p.public-key.pem
```

A 200 with `content-type: application/x-pem-file` (or `application/octet-stream`) means Tesla can
fetch it. Do this before attempting to pair, since a failed fetch produces an unhelpful error in
the mobile app.
