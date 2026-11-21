# Quickstart: Loomux on one Docker host

**You need:**
- a Linux machine with Docker Engine 20.10 or newer and Docker Compose v2 (`docker compose version`), and about
  3 GB of free disk;
- an API key for the **router model**, the small model that reads your messages and picks where work goes:
  Anthropic's API, or any OpenAI-compatible endpoint that supports forced tool calls (`tool_choice`);
- a **Claude Code or Codex account** to sign the agent in with.

Time: about **10 minutes** to a signed-in agent on a 50 Mbit/s connection, most of it the agent image download
(517 MB, 2 GB unpacked) and the sign-in. A slower connection adds to the download. See [Timings](#timings).

Everything runs on this one machine: `loomuxd` in one container, and the machine your agents work on in another
container, which Loomux creates through the host's Docker engine.

> [!WARNING]
> **This gives Loomux control of your Docker engine, which is root on this host**, and whoever logs in to Loomux
> can run commands in its machines. Use a machine or VM you don't mind that for, and keep the web UI on
> `localhost` as set up here. Read [Security](../README.md#security) before exposing it anywhere else.

This is a try-out on one host. To run agents on your own servers over SSH, or on Kubernetes, see
[Installing Loomux](deploy/install.md).

## 1. Write the configuration

```sh
mkdir loomux && cd loomux
curl -fsSLO https://raw.githubusercontent.com/Loomux/server/main/deploy/quickstart/compose.yaml
```

Pick the newest version from [Releases](https://github.com/Loomux/server/releases), then make the `.env` file
beside `compose.yaml`. It asks for the password you'll log in with and for your router API key, and prints
neither:

```sh
LOOMUX_VERSION=0.3.155
printf 'Choose a Loomux password: '; stty -echo; read -r PW; stty echo; echo
printf 'Router model API key: '; stty -echo; read -r KEY; stty echo; echo
HASH=$(printf '%s\n' "$PW" | docker run --rm -i ghcr.io/loomux/server:$LOOMUX_VERSION -hash-password)
umask 077
cat > .env <<EOF
LOOMUX_VERSION=$LOOMUX_VERSION
DOCKER_GID=$(stat -c %g /var/run/docker.sock)
LOOMUX_AUTH_PASSWORD_HASH='$HASH'
LOOMUX_MASTER_KEY=$(head -c 32 /dev/urandom | base64)
LOOMUX_ROUTER_PRIMARY_PROVIDER=anthropic
LOOMUX_ROUTER_PRIMARY_MODEL=claude-haiku-5-5
LOOMUX_ROUTER_PRIMARY_API_KEY=$KEY
EOF
unset PW KEY HASH
```

For an OpenAI-compatible endpoint instead, set `LOOMUX_ROUTER_PRIMARY_PROVIDER=openai`, add
`LOOMUX_ROUTER_PRIMARY_BASE_URL=https://…/v1`, and put that endpoint's model name in `LOOMUX_ROUTER_PRIMARY_MODEL`.

**Keep a copy of `.env`.** `LOOMUX_MASTER_KEY` encrypts Loomux's credential vault, the plugins' secrets and the
machines' SSH keys; without it, those can't be decrypted from a restored database.

## 2. Start it

```sh
docker compose up -d
curl -s http://localhost:8080/api/v1/health
```

The health check answers `{"status":"healthy",…}` within a few seconds. Port 8080 taken? Add `LOOMUX_PORT=8090` to
`.env` and use that port everywhere below.

At its first start Loomux installs the Docker plugin and creates one machine, `dev` (both are declared in
`compose.yaml`). The machine's first start pulls the agent image, `ghcr.io/loomux/agent`, which carries
Claude Code, Codex, tmux and git. That takes a few minutes.

## 3. Log in and wait for the machine

Open <http://localhost:8080> and log in with your password. Open **Machines**: `dev` shows the image pull's
progress, then **running**. From a terminal:

```sh
docker ps --filter label=loomux.io/target-name=dev
```

lists the machine's container once it exists.

## 4. Sign the agent in, once

The sign-in is kept on the machine's volume, so this is a one-time step. In a terminal:

```sh
docker exec -it $(docker ps -qf label=loomux.io/target-name=dev) claude auth login
```

Open the link it prints, approve, and paste the code back at `Paste code here if prompted >`. **The code doesn't
show when you paste it**: paste it once and press Enter. `claude auth status` then says `"loggedIn": true`. For
Codex, run `codex login --device-auth` instead.
<!-- When loomux/server#558 (agent sign-in by tmux attach) is released, this step becomes:
     `docker compose exec loomuxd loomuxd agents sign-in -target dev -agent claude-code` (with LOOMUX_TOKEN),
     then the docker exec … tmux attach line it prints. Timed in the LOOM-309 run. -->

## 5. Ask for something

In the web UI, start a **new conversation** and send:

> In a new workspace called hello on dev, create hello.txt containing "hi" and tell me what you did.

The router picks the machine, Loomux creates the workspace and starts Claude Code there in its normal
interactive mode, and the answer comes back into the conversation. If the agent stops at a prompt Loomux
recognises, such as a permission request, it shows up as a card with **Approve / Deny / Reply**.

## 6. Look at the real session

The agent runs in tmux, not headless. Attach to it and watch or type:

```sh
docker exec -it $(docker ps -qf label=loomux.io/target-name=dev) tmux -L loomux attach
```

Detach with `Ctrl-b d`; the agent keeps running. The task's page in the web UI shows the same command for a
specific session.

## Stop, or remove everything

```sh
docker compose stop          # stop the server; the machine keeps running until you stop it
docker compose up -d         # start again, nothing lost
```

To remove it all (the server's database, the machine and its data). The last three commands remove every
machine any Loomux Docker plugin made on this host, so on a host you share, check what they list first:

```sh
docker compose down --volumes
docker ps -aqf label=loomux.io/managed-by=loomux-plugin-docker | xargs -r docker rm -f
docker volume ls -qf label=loomux.io/managed-by=loomux-plugin-docker | xargs -r docker volume rm
docker network rm loomux-agents 2>/dev/null || true
```

Deleting the machine in the web UI alone doesn't last: `compose.yaml` declares it, so the next start makes it
again, empty. Take it out of `LOOMUX_PLUGINS_SEED` first to remove it for good.

## Next

- More machines: **Machines → Create a machine**, with sizes `small`, `medium` and `large`.
- Your own servers over SSH: [targets](deploy/targets.md) and [SSH](deploy/ssh.md).
- Kubernetes, a pod per machine: [plugins](deploy/plugins.md).
- Notifications on your phone: set `LOOMUX_NTFY_URL` and `LOOMUX_NTFY_TOPIC` ([configuration](deploy/container.md#configuration)).
- Running it for real (TLS, backups, upgrades): [install](deploy/install.md), [operations](deploy/operations.md),
  [backup and restore](deploy/backup-restore.md).

## Troubleshooting

- **The health check gets no answer, and `docker compose ps` shows the server restarting**: `docker compose logs
  loomuxd` says why. `router config: … LOOMUX_ROUTER_PRIMARY_API_KEY is not set` means the key line in `.env`
  is empty: add the key and run `docker compose up -d` again.
- **The Docker plugin is in `error`** (on **Plugins**, and in `docker compose logs loomuxd`) with `can't reach
  the docker engine … connect: permission denied`: `DOCKER_GID` in `.env` isn't the socket's group. Compare it
  with `stat -c %g /var/run/docker.sock`, fix it, and run `docker compose up -d` again.
- **The machine stays `error`**: its reason is on the Machines page; `docker compose logs loomuxd` has the
  detail.
- **macOS or Windows (Docker Desktop):** not tested. The compose file relies on host networking.

## Timings

Measured on 2026-11-21 with release 0.3.155, on a Linux laptop with Docker 29.9, following this page from an empty
directory:

| Step | Took |
|---|---|
| 1. Write the configuration | a few seconds of commands, plus typing a password and a key; the server image is a 38 MB download |
| 2. Start it | 14 s to a healthy server |
| 3. The machine | 30 s once the agent image is on the host. The image is a 517 MB download, then about 1.5 min to unpack |
| 4. Sign the agent in | 4 min, by a person, the browser sign-in included |
| 5. Ask for something | not measured yet |

That is about 10 minutes to a signed-in agent on a 50 Mbit/s connection. The downloads alone take about
15 minutes at 5 Mbit/s.
