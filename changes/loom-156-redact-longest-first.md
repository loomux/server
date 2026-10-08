### Security

- Redaction replaces the longest vault values first (LOOM-156): when one
  credential's value is a prefix or part of another's, the longer one's
  remainder no longer leaks into relayed output, transcripts or
  notifications.
