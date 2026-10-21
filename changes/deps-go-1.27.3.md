### Security

- Built with Go 1.27.3, which fixes GO-2026-6809 (`crypto/x509` now
  distrusts compromised intermediate certificates); govulncheck reports
  it against Go 1.27.2, reachable through the web bundle's attestation
  check.
