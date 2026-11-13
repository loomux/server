### Security

- Built with Go 1.27.4, which fixes GO-2026-6859 (an HTTP/1 server crash
  from a `MaxBytesReader` header race in `net/http`); govulncheck reports
  it against Go 1.27.3, reachable through the API's request-body limit.
