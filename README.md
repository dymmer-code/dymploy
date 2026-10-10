# dymploy

A small CI/CD for servers: it receives repository events (webhooks), builds in Docker according to
the `dymploy.yaml` of the repository, extracts a transportable artifact (`tar.gz`) and deploys it.

One Go binary, two modes:

- `dymploy serve` — the daemon: HTTP listener for webhooks and the API, job queue, control socket.
- `dymploy status`, … — the CLI, which talks to the running daemon (no graphical interface).

It is independent of the repository provider (GitHub first; Bitbucket, GitLab, SourceHut later) and of
where it is installed: any organization can run its own.

> Status: **early development**. Documentation: see the `docs` directory.

## Quick start

```sh
go build -o dymploy .
cat > config.yaml <<EOF
listen: 127.0.0.1:8080
control_socket: /tmp/dymploy.sock
data_dir: /tmp/dymploy-data
EOF
./dymploy serve --config config.yaml &
./dymploy status --config config.yaml
curl -s http://127.0.0.1:8080/healthz
```

## Configuration

Loaded from `--config` (env `DYMPLOY_CONFIG`, default `/etc/dymploy/config.yaml`). Unknown keys are an
error. Every key is optional.

| Key | Default | Meaning |
|---|---|---|
| `listen` | `127.0.0.1:8080` | HTTP address for webhooks and the API |
| `control_socket` | `/run/dymploy/control.sock` | Unix socket (mode 0600) used by the CLI |
| `data_dir` | `/var/lib/dymploy` | repositories, artifacts, history and logs (mode 0750) |

## Configuration files and reload

The configuration is `config.yaml` plus every `config.d/*.yaml` next to it, read in alphabetical order (see
`examples/config/`). The split is meant for deployment tools: what belongs to the installation stays in
`config.yaml`; data that is generated elsewhere (for example the policy, produced by another tool) goes in its own
drop-in file that can be replaced at any time.

- `policy` rules are added up and `webhooks` entries are combined (each name once); every other setting may be
  given in one file only. Giving it twice is an error naming both files, so what is in force never depends on
  which file comes last.
- Unknown keys, anchors and aliases are errors, per file. A file or directory that others can write is refused.
- Secrets are never read from the drop-ins' contents: `secret_file`, `private_key_file` and the like are paths to
  files on this machine.
- `dymploy reload` (or `SIGHUP`) re-reads everything while the daemon runs. The **policy** and the **targets** are applied; if any
  other setting changed the reload is refused as a whole and the daemon says which settings need a restart. A
  configuration that does not load leaves the previous one in force, and the reason is returned to the caller.
  Jobs already running are not affected.

## Webhooks

Each provider that may send events is listed under `webhooks` with the file that holds its webhook
secret (absolute path; it must not be readable by other users, `chmod 600`). The daemon serves
`POST /hooks/<provider>`. Nothing is built unless a `policy` rule allows it: the allow-list lives in
this file, never in a repository.

```yaml
webhooks:
  github:
    secret_file: /etc/dymploy/github-webhook-secret
policy:
  - provider: github
    owner: globex
    repositories: ["*"]        # patterns (path.Match); "*" = every repository of the owner
    branches: [main, "release/*"]   # "*" = every branch, including feature/x
    tags: ["v*"]               # tags are only built when listed
    installations: [123456]    # optional: only events from these installations
webhook_max_body_bytes: 5242880    # default 5 MiB
webhook_rate_per_minute: 600       # default 600, 0 disables
```

Answers: `202` accepted · `200` ignored (ping, other events, deleted refs) or duplicate delivery ·
`400` malformed · `401` not authenticated · `403` outside the policy · `405` not POST · `413` too large ·
`429` rate limited · `503` queue full (retry).

## The `dymploy.yaml` file

Every repository that dymploy builds has a `dymploy.yaml` in its root. It says how to build (in Docker), what to
pack, and where to deploy; see `examples/` for complete files (a release, a static site, a book). Check yours before pushing, and give your editor the schema:

```sh
dymploy validate            # looks for dymploy.yaml in the current directory; reports every problem at once
dymploy validate path/to/dir-or-file
dymploy schema > dymploy.schema.json   # JSON Schema for editor completion and validation
```

```yaml
version: 1
build:
  image: {from: "debian:13"}        # or: {dockerfile: docker/Dockerfile.build}
  steps:
    - run: make
artifact:
  paths: [out/app]                  # stored in the tar.gz under its base name (app)
deploy:
  target: production                     # a NAME configured on the dymploy server, never a host or a user
  steps:
    - run: tar -xzf "$DYMPLOY_ARTIFACT" -C /srv/apps/app --strip-components=1
```

The file is treated as untrusted: unknown keys, anchors and aliases are errors, every size is bounded, paths
cannot leave the repository, and credentials and machines can never be written in it. The full reference is in `docs/dymploy-yaml.md`.

## Cloning repositories

`runner.kind: clone` checks out the commit of the event in a working copy under `<data_dir>/repos/<provider>/<owner>/<repo>`
(created if missing, updated otherwise, cleaned before every build, one user at a time). Everything git-related
lives in `internal/checkout`:

- Credentials are read-only and short-lived, issued by the provider, and travel to git in environment variables scoped
  to the repository's own host: never in arguments, URLs, logs or files (the remote is stored without them).
- git runs with a private `HOME`, the user's and the system's configuration ignored, hooks disabled, submodules
  fetched only when asked (`params: {submodules: true}`) and every value validated before it reaches git.
- URLs must be `https`; the provider builds them, they are never taken from the webhook payload.
- Working copies not used for `repo_keep_days` (default 14) are removed.

For GitHub, private repositories need an App: give the webhook entry `app_id` and `private_key_file` (the `.pem`, mode
`0600`). dymploy signs a short-lived JWT and asks GitHub for an installation token limited to **that one repository** and
to **reading its contents**.

```yaml
webhooks:
  github:
    secret_file: /etc/dymploy/github-webhook-secret
    app_id: 123456
    private_key_file: /etc/dymploy/github-app.pem
runner:
  kind: clone
  params: {depth: 1, submodules: false}   # both optional; unknown keys are an error
repo_keep_days: 14
```

## Queue and runners

Accepted events become jobs. A job is named after its repository (`github/owner/repo`): jobs with the
same name never run at the same time, and at most `queue.max_concurrent` run at once. Each job has a
state (`queued`, `running`, `succeeded`, `failed`, `canceled`, `interrupted`), a history and a log
(`<data_dir>/logs/<job>.jsonl`, one line per entry, written as it happens, capped at
`queue.log_max_bytes`). State lives in `<data_dir>/state/jobs.db`; only one daemon may own it. Jobs that
were running when the daemon stopped are marked `interrupted` (never repeated blindly); jobs that were
waiting resume in order.

```yaml
queue:
  max_concurrent: 2     # jobs running at once
  max_pending: 1000     # jobs allowed to wait; beyond that webhooks get 503
  on_busy: queue        # queue (wait) | coalesce (replace the waiting one) | reject
  log_max_bytes: 10485760
  keep_jobs: 500        # history bounds for finished jobs (0 = no limit)
  keep_days: 30
runner:
  kind: dryrun          # how jobs are executed; see below
  params: {}            # handed, untouched, to the runner: only the runner knows what it means
```

The queue does not know how work is done. It hands each job to the **runner** registered for its
`kind` through the contract in `internal/runner`: the runner receives opaque `Params` (from the
configuration) and `Input` (the event), narrates the execution continuously through a `Reporter`
(stdout, stderr and system steps, in order) and returns an exit code or an error. Docker is just one
implementation of that contract. `dryrun` executes nothing and says what it would do, which lets the
whole chain be checked without side effects; `clone` checks out the commit; `docker` builds it.

## Building in Docker

`runner.kind: docker` clones the commit like `clone`, reads `dymploy.yaml` from it (see `dymploy validate`),
runs each `build.steps` entry in its own container and packs `artifact` into
`<data_dir>/artifacts/<job id>/<name>.tar.gz`, logging its size and SHA-256. It needs the `docker` command and
a reachable engine (checked at every job, so Docker can be started after dymploy).

```yaml
runner:
  kind: docker
  params:
    depth: 1              # shallow clone (optional)
    limits:               # the most a build may use, and what it gets when its definition says nothing
      memory: 4g          # default 4g
      cpus: 2             # default 2
      timeout: 1h         # default 1h
```

A definition that asks for more than these limits is refused (exit code 2, saying which limit); one that asks for less gets
less. Submodules are not supported by this runner.

- The repository is mounted at `/work`; steps run with `sh -ec`, as the daemon's own uid:gid (so what they write
  can be read and cleaned up) with `HOME=/tmp`. Declare `build.cache` for anything that must survive between builds
  (`<data_dir>/cache/<provider>/<owner>/<repo>/<name>`, mode 0700).
- Containers are unprivileged: all capabilities dropped, `no-new-privileges`, bounded processes, no swap, no
  access to the Docker socket, no devices, no published ports. The `.git` directory is mounted **read-only** inside the
  build: the daemon runs git on the working copy afterwards, and a configuration or hook planted by a build would otherwise
  run on the host. `build.network: none` removes the network.
  `build.limits` sets cpus, memory and the time limit; exceeding the time makes the job fail with exit code 124.
- Environment: `build.env`, then the step's `env`, then `DYMPLOY_COMMIT`, `DYMPLOY_SHORT_COMMIT`, `DYMPLOY_OWNER`,
  `DYMPLOY_REPO`, `DYMPLOY_JOB` and `DYMPLOY_BRANCH` or `DYMPLOY_TAG`, which always win. No secret of the server is passed.
- `build.image.dockerfile` builds an image for the job (and removes it afterwards); `from` pulls one. Both must stay
  inside the repository: links that lead out are refused.
- The artifact stores each path under its base name; links must be relative and stay inside what is packed, special
  files are refused, owners are normalized and the order is fixed, so the same tree gives the same archive.
  `artifact_keep_days` (default 14) removes old ones.
- Exit codes of a job that failed for reasons of the repository: 2 invalid or missing `dymploy.yaml`, 3 the build
  worked but the artifact could not be packed, 124 time limit, anything else is the failing step's own code. An
  engine that is down, an image that cannot be pulled or a cancelled job are errors of the runner instead.

## Deploying

If `dymploy.yaml` has a `deploy` section, the job ends with it: the artifact is delivered to the named **target** and the
steps run there. The target is defined in the daemon's configuration (`targets`, see `examples/config/`), never in the
repository:

- `host`, `port`, `user`; `key_file` (a private key without passphrase, mode 0600 at most) and `known_hosts` (the host key
  must be listed: nothing is trusted on first use, and a changed host key is refused).
- `allow`: the repositories, branches and tags that may deploy there (same rules as `policy`). A definition that asks for
  a target that does not exist or that it may not use fails **before anything is built** (exit code 4).
- A target whose files are missing or unusable stops the daemon from starting, and a reload that contains one is refused
  as a whole.

The job goes build → pack (`artifact.json` is written next to the archive with its SHA-256, commit and job) → deploy:
the archive is read back through its record, uploaded to `upload_dir` over the SSH connection (no SFTP needed), its
SHA-256 is **verified on the target before anything runs**, then `deploy.steps` run in order with `sh -ec`, then `check`
(retried `attempts` times), then `after`. The upload is removed at the end. `DYMPLOY_ARTIFACT` (the remote path),
`DYMPLOY_ARTIFACT_SHA256`, `DYMPLOY_TARGET`, `DYMPLOY_COMMIT`, `DYMPLOY_SHORT_COMMIT`, `DYMPLOY_OWNER`, `DYMPLOY_REPO`,
`DYMPLOY_JOB` and `DYMPLOY_BRANCH` or `DYMPLOY_TAG` are available to the steps; secrets and database migrations stay on
the target.

Exit codes: 4 target missing or denied, 5 a deploy step failed, 6 the health check never passed, 7 an `after` step failed
(the deployment itself is done), 124 `deploy.timeout` ran out. A target that cannot be reached or a damaged upload is an
error of the runner instead. **There is no automatic undo**: on failure whatever the steps already did stays as it is and
the log says so. The format has no `rollback` section on purpose (re-running the deploy of a previous job is being considered instead).

Nothing outside `internal/runner/docker` knows about containers (`internal/architecture` enforces it, and the same for SSH, which only `cmd` and `internal/deploy/sshremote` know). Tests against a
real engine are tagged: `go test -tags integration ./internal/runner/docker/` (and `./internal/deploy/sshremote/` against a real OpenSSH, see the file's header) (set `DYMPLOY_IT_DIR` under your home when
the engine runs in a VM such as Colima).

## Looking at the jobs

The same binary talks to the running daemon through the control socket (`--socket`, or the one in the configuration):

```sh
dymploy jobs [--state running,queued] [--name github/owner/repo] [--limit 20] [--json]
dymploy job <id>                 # state, exit code, error, timings
dymploy logs <id> [-f]           # -f follows until the job ends (lines: "#" dymploy, "!" stderr)
dymploy cancel <id>              # a waiting job never runs; a running one is stopped
dymploy retry <id>               # a finished job again, as a new job
dymploy watch [--json]           # everything, live: what is waiting or running, then every change and log line
```

The API behind those commands is `GET /v1/jobs`, `/v1/jobs/{id}`, `/v1/jobs/{id}/logs?after=&limit=`, `POST /v1/jobs/{id}/cancel`
and `/retry`, and the live channel `GET /v1/events` (a WebSocket). The channel starts with a `snapshot` of the jobs waiting or
running and then sends `queued`, `started`, `output` and `finished` messages as JSON. With `?job=<id>&after=<seq>` it first
replays that job's log from the sequence number and follows the job until it finishes, so a client that reconnects loses
nothing: every log line carries its `seq`, and a gap means the client was too slow and can read the log again. The
parameters and input of a job (which can mention repository details) are never part of the API.

Other tools can use the **read-only** part on the public listener, under `/api/v1/`, when `api.token_file` is set
(`Authorization: Bearer <token>`; see `examples/config/`). Without the token file nothing is mounted. Cancelling and retrying
stay on the control socket. Run it behind the reverse proxy over TLS; `api.allowed_origins` lists the web origins allowed to
open the channel from a browser.

## Development

```sh
go vet ./... && go test -race -cover ./...
```

See `AGENTS.md` for the working conventions.

## License

MIT
