# Security policy

This policy covers the Loomux project: [`loomux/server`](https://github.com/loomux/server) (`loomuxd`, its
bundled plugins and the images `ghcr.io/loomux/server`, `ghcr.io/loomux/agent`, `ghcr.io/loomux/plugin-docker`
and `ghcr.io/loomux/plugin-kubernetes`), [`loomux/web`](https://github.com/loomux/web) and
[`loomux/tui`](https://github.com/loomux/tui). It is not about the commercial product at loomux.dev, which is
unrelated to this project.

How Loomux is meant to be deployed, and what it trusts, is in the README's
[Security section](README.md#security). Read it first: some things that look like vulnerabilities are the
documented model of a single-user v1.

## Supported versions

Loomux is pre-1.0 and every release is a GitHub pre-release. Only the **newest release** gets security fixes.
A fix lands on `main` and ships as the next patch release; there are no backports to older versions, so
upgrade to get it.

| Version | Supported |
|---|---|
| The newest `0.x` release | yes |
| Any older release | no: upgrade |

## Reporting a vulnerability

**Please don't open a public issue, pull request or discussion for a vulnerability.** Report it privately
through GitHub, in the repository it belongs to:

- server, plugins, images: <https://github.com/loomux/server/security/advisories/new>
- web client: <https://github.com/loomux/web/security/advisories/new>
- terminal client: <https://github.com/loomux/tui/security/advisories/new>

Not sure which? Use the server's. Include, as far as you can:

- the version (`GET /api/v1/version`, or the image tag and digest);
- how it was deployed (Docker, Compose, Kubernetes, which plugins);
- the steps to reproduce, and what an attacker gains;
- whether the attacker needs a login, a session token, access to a target, or nothing.

Test only against an instance you run yourself. Never against someone else's.

## What to expect

Loomux is maintained by one person, so these are targets, not guarantees:

- an acknowledgement within **7 days**;
- an assessment (confirmed or not, and how severe) within **14 days**;
- for a confirmed high or critical issue, a fixed release within **30 days**, sooner when it's simple.

The fix is released first. A GitHub security advisory follows (with a CVE when it warrants one), and it credits
you unless you'd rather not be named. Please keep the details private until that advisory is published. If a
fix will take longer, you'll hear why and when.

## Scope

**In scope**, for example:

- getting past the login, or taking over a session token, without the password;
- cross-site scripting, request forgery, or a header or CSP weakness in the web client or the API;
- reading the credential vault, the plugins' secrets or the machines' SSH keys without a session, or
  decrypting them without `LOOMUX_MASTER_KEY`;
- message text, router-model output or agent output that makes `loomuxd` act beyond what the trust model in the
  README allows (for example, run something on a target the user didn't route to);
- bypassing SSH host-key verification;
- a machine a plugin makes getting more than its documented configuration gives it (a capability, a host
  mount, a host namespace, another machine's volume);
- a plugin acting beyond the permissions its manifest declares;
- the release pipeline: a way to publish an image, plugin or web bundle that passes attestation checks but
  wasn't built from this project's `main`.

**Out of scope**, because it is the documented model or someone else's software:

- anything that needs a valid login or session token: in v1, that user can run commands on every registered
  target by design;
- what an agent does inside the permission mode it runs in. Bugs in Claude Code's or Codex's own permission
  checks or sandbox go to Anthropic or OpenAI;
- what the router-model provider receives, as the README describes;
- the Docker plugin's engine access being root on its Docker host;
- no multi-factor authentication in Loomux itself (put an authenticating proxy in front);
- findings that need root on the server's host or on a target first;
- denial of service by sheer volume against an instance exposed to the internet.

A gap in the **best-effort redaction** of vault values from relayed or stored output is worth fixing but isn't
treated as a vulnerability: report it as a normal issue, without the secret.
