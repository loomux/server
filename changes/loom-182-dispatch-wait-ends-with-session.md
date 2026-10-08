### Security

- A blocking dispatch (`POST /api/v1/dispatch?wait=true`) now ends with its session (LOOM-182), as a conversation stream does since LOOM-144: logging out or revoking the device answers the waiting request `401` at once instead of delivering the turn's reply to the revoked token. The turn itself carries on, and its result is in the conversation.
