### Fixed

- A missing static file — anything under `/assets/`, or a path ending in
  an extension such as `.js` — is a `404` instead of the app shell
  (LOOM-158). A browser holding the bundle from before a web update now
  sees a failed chunk load it can retry, not HTML served as JavaScript.
  Client-side routes still get the shell.
