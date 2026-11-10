# ACP agent sandbox (runner)

Jaz can start every ACP agent process in its own rootful podman container on
the host instead of as a child process of the Jaz server. Jaz stays the
orchestrator: ACP still runs over the agent's stdio, so launch-time prompts and
configs keep native-agent parity.

```
Jaz container                         host
acp.openConn ─exec─> jaz sandbox-attach ──unix socket──> jaz runner ──> podman run --rm -i --init …
                     (stdio, signals,                     (policy, mounts,
                      exit status)                         host path mapping)
```

`jaz sandbox-attach` replaces the agent process locally, so Jaz keeps its
process supervision (stdio, SIGTERM with grace, exit status). The runner is the
trust anchor: it decides what may be mounted, translates Jaz paths to host
paths, and resolves symlinks on the host so a request cannot escape the allowed
roots.

## Jaz side

```yaml
jaz:
  sandbox:
    runtime: runner # empty (default) runs agents as local child processes
    socket: /run/jaz-runner/runner.sock
    paths: [/var/lib/jaz/workspaces/sandbox-canary] # optional canary scope
```

With `paths`, only agents whose working directory is under one of them run in
the sandbox; everything else stays local. While local agents remain, they reach
Jaz MCP through `--agent-url` too, so include Jaz's own address in
`--agent-networks` (e.g. `10.0.92.10/32`).

Server flags (pass from the container entrypoint):

- `--agent-url http://<jaz-ip>:5299` — the Jaz URL agents use for Jaz MCP
  endpoints, since `127.0.0.1` inside a session is the session itself.
- `--agent-networks <cidr>[,<cidr>]` — where sessions connect from; the
  backend key in their MCP config is accepted from there for the internal MCP
  endpoints only, as it is from loopback. Every agent therefore holds the
  backend key, and session IDs in MCP headers are not secrets, so any session
  can claim any session ID, exactly as any local agent can.

Mount the runner socket directory into the Jaz container
(`/run/jaz-runner:/run/jaz-runner`).

## Runner (host)

`jaz runner --config /etc/jaz/runner.yaml` (systemd, as root). Paths in
`readonly`, `readwrite` and `writable` are Jaz paths; `mounts` maps them to the
host.

```yaml
socket: /run/jaz-runner/runner.sock
socketowner: "10001:999"          # Jaz user in its container
image: localhost/jaz:vX.Y.Z        # the Jaz backend image, in podman's store
network: jaz-sessions             # podman network for sessions
dns: [10.0.0.1]
hosts: ["tbot:10.0.1.97"]
user: "10001:999"
home: /var/lib/jaz-agent          # HOME for agents, outside Jaz's own state
stoptimeout: 10s
mounts:
  - {container: /var/lib/jaz, host: /srv/jaz/data}
  - {container: /var/lib/jaz/workspaces/work, host: /srv/jaz/workspaces}
  - {container: /var/lib/jaz-agent, host: /srv/jaz/agent-home}
readonly:                         # every session, read-only
  - /var/lib/jaz/.local/share/mise
  - /var/lib/jaz/skills
  - /var/lib/jaz/AGENTS.md
  - /var/lib/jaz/workspaces
readwrite:                        # every session, writable
  - /var/lib/jaz-agent
  - /var/lib/jaz/memory
writable:                         # roots a launch may request as writable
  - /var/lib/jaz/workspaces
  - /var/lib/jaz/acp
args: ["--memory=4g", "--pids-limit=4096", "--security-opt=apparmor=jaz-session",
  "--security-opt=seccomp=/etc/jaz/seccomp-docker-default.json"]
```

With `workspaces` read-only and writable per launch, a session writes only its
own working directory. A working directory under a read-only parent needs its
mount point to exist in the parent's host directory. The docker default seccomp
profile denies user namespaces and mounts, which podman's default allows.

Host files are in [recipes/agent-runner](recipes/agent-runner): the systemd unit,
the `jaz-session` AppArmor profile (`/etc/apparmor.d/jaz-session`, load with
`apparmor_parser -r`) and a containers.conf drop-in that keeps podman's
container events out of journald.

The AppArmor profile matters on Ubuntu 24.04+: crun has its own profile there,
so container processes run under a stacked label (`crun//&<profile>`), and
podman's `containers-default` denies every signal to that label — `--init`
cannot forward SIGTERM and an agent cannot interrupt its own tool commands.
`jaz-session` is `containers-default` with signal and ptrace peers that match
the stacked label.

Each session gets: the requested working directory and agent state home
(writable), the agent command's directory and per-launch config directory
(read-only, when they live on a mapped path), the static mounts above, a fresh
`/tmp`, `--userns=auto` with idmapped volumes (files stay owned by the Jaz user,
the process runs as an unprivileged host UID), `--init`, no logs on the host.
Environment values reach the container through podman's own environment, never
its argv.

Containers are labelled `jaz.runner=1`; the runner removes leftovers at start
and stops a container when its Jaz client disappears.

## Not yet covered

- Previews: the preview proxy only targets Jaz's loopback, while a sandboxed
  agent's dev server listens in its own container.
- Login flows and the Codex token refresh run in the Jaz container (they write
  the same mounted agent homes).
- Agents lose what only the Jaz container had: Machine ID certificates,
  `cp-grant`, kube configs — by design.
