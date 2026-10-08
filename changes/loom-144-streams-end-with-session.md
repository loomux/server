### Security

- A conversation stream now ends with its session (LOOM-144): logging out or revoking a device (`DELETE /api/v1/sessions/{id}`) closes that session's open streams at once, and a stream whose session expires closes at its next heartbeat. Before, a stream opened with a token kept delivering events after the token was revoked.
