### Security

- Browser hardening headers on every response (LOOM-143): `Content-Security-Policy` (the web client may run only its own scripts, connect only to this server, and can't be framed), `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `Cache-Control: no-store` on the API, and `Strict-Transport-Security` when served over HTTPS (behind a proxy, per its `X-Forwarded-Proto`). The web client's own inline script is allowed by hash, computed from the `index.html` actually served.
