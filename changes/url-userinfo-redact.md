### Security

- Credentials in a URL (`https://user:password@host/…`, or a bare token
  as the user) are redacted wherever Loomux redacts text: what the relay
  model sees, transcripts and the dispatch audit trail. An SSH URL's
  plain `git` user is kept.
