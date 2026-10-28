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
webhook_rate_per_ip_per_minute: 60 # default 60, 0 disables: each client address has its own budget
trusted_proxies: [192.0.2.10/32]   # reverse proxies whose X-Forwarded-For is believed (empty: the connection's address)
```

The per-address limit is checked **before** the shared one counts anything, so a single address cannot use up the budget
everybody else shares. Behind a reverse proxy every connection comes from the proxy, so list it in `trusted_proxies`: only then
is `X-Forwarded-For` read (from the right, skipping the proxies) to know the real client, for the limit and for the logs.
Anyone else writing that header is ignored.

Answers: `202` accepted · `200` ignored (ping, other events, deleted refs), duplicate delivery or stale commit ·
`400` malformed · `401` not authenticated · `403` outside the policy · `405` not POST · `413` too large ·
`429` rate limited · `503` queue full, or the provider could not be asked (retry).

### Late, out-of-order and replayed deliveries

A signature proves who sent a webhook, not when. A captured delivery can be sent again later, and deliveries can arrive out of
order. So, before queueing a push, dymploy asks the provider where the branch or tag points now and **ignores the event when
its commit is no longer the tip** (`200 stale`; a deleted branch counts too). If the provider cannot be asked, nothing is queued
(`503`, so it can be delivered again). A person starting a job by hand with `dymploy queue` chose the commit, so that is never
checked. It is on by default and needs the provider to be able to answer (on GitHub, the App); a provider that cannot is not
checked, and the log says so at start. To switch it off for one provider:

```yaml
webhooks:
  github:
    verify_tip: false
```

## Trying a definition without pushing anything

```sh
cd my-repository
dymploy validate                                  # the file is well formed
dymploy try                                       # build and pack it here, like a job would
dymploy try --deploy --config etc/config.yaml     # and deliver it to the target the file names
```

`dymploy try` needs no webhook, GitHub App, daemon or clone: it takes what git tracks in the directory plus your uncommitted
changes and new files that are not ignored (so a `dymploy.yaml` that is not committed yet works, and secrets that are git-ignored
never reach the build), copies them into a throw-away directory and runs them with the same runner as the daemon (the same
containers, limits, caches and artifact), showing the log as it happens. Ctrl-C stops the containers properly. The deploy
section is **left alone** unless you pass `--deploy`, which needs a configuration with the target; the target's `allow`
rules are applied to the repository and branch the run pretends to be (taken from `origin` and the current branch, or `--name`
and `--ref`), so a try cannot do what a push could not. Artifacts and caches go under the user cache directory (`--out`).

`examples/rehearsal/` starts a stand-in for the target (Debian 13 with sshd, a deploy user that may restart one service, a
PostgreSQL) so that a whole build-and-deploy can be watched on a laptop before the real server exists.

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

`artifact` and `deploy` are optional. A job with neither only checks the code (`examples/tests-only.yaml`): it builds, runs
its steps and succeeds or fails with them; a `deploy` always needs an `artifact`.

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

Accepted events become jobs. A job is named after its repository (`github/owner/repo`) and every run of it is numbered, as in
any CI: `github/acme/shop #12` is the twelfth run of that repository, and numbers are never reused (`dymploy job
github/acme/shop#12`, `logs`, `abort` and `retry` take that as well as the id). Jobs with the
same name never run at the same time, and at most `queue.max_concurrent` run at once. Each job has a
state (`queued`, `running`, `succeeded`, `failed`, `aborted`, `interrupted`), a history and a log
(`<data_dir>/logs/<job>.jsonl`, one line per entry, written as it happens, capped at
`queue.log_max_bytes`). State lives in `<data_dir>/state/jobs.db`; only one daemon may own it. A job that was
stopped on purpose is `aborted` and says why (`stop_reason`); one that was running when dymploy *died* (a crash, a power cut)
is found at the next start and marked `interrupted`, never repeated blindly; jobs that were waiting resume in order.

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
      nofile: 65536       # open files of the containers; unset keeps the container engine's own limit
    cache_scope: repository   # who shares a build cache: "repository" (default) or "branch"
cache_keep_days: 30       # caches not used for this long are removed; 0 keeps them
cache_max_mb: 5120        # a cache that grows past this is emptied (the next build starts from nothing); 0 = no limit
```

A definition that asks for more than these limits is refused (exit code 2, saying which limit); one that asks for less gets
less. Submodules are not supported by this runner.

- The repository is mounted at `/work`; steps run with `sh -ec`, as the daemon's own uid:gid (so what they write
  can be read and cleaned up) with `HOME=/tmp`. Declare `build.cache` for anything that must survive between builds
  (`<data_dir>/cache/<provider>/<owner>/<repo>/<name>`, mode 0700). A cache is shared by every build of the repository unless
  `cache_scope: branch`, which gives each branch its own: a branch the policy trusts less cannot plant something in the cache
  that a more trusted one builds from. Caches are cleaned once an hour and at start: those not used for `cache_keep_days`
  go, and so does one larger than `cache_max_mb` (a cache a build is using is never touched).
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

If `dymploy.yaml` has a `deploy` section, the job ends with it: the artifact is delivered to the named **target**. A
target is a machine, where the steps run (below), or a bucket of an S3 server (`kind: s3`), where the files of the
artifact are put: see `docs/dymploy-yaml.md`, *Buckets*. Either way the target is defined in the daemon's configuration
(`targets`, see `examples/config/`), never in the repository. For a machine, the steps of `dymploy.yaml` run there:

- `host`, `port`, `user`; `key_file` (a private key without passphrase, mode 0600 at most) and `known_hosts` (the host key
  must be listed: nothing is trusted on first use, and a changed host key is refused).
- `allow`: the repositories, branches and tags that may deploy there (same rules as `policy`). A definition that asks for
  a target that does not exist or that it may not use fails **before anything is built** (exit code 4).
- `secrets` (optional): a command that prints the target's secrets, for definitions with `deploy.secrets: true`. dymploy
  runs it (no shell, minimal environment), uploads the output to the target next to the artifact (mode 0600, SHA-256
  verified, path in `DYMPLOY_SECRETS_FILE`), never logs it, and removes it when the deployment ends. See
  `docs/dymploy-yaml.md`.
- A target whose files are missing or unusable stops the daemon from starting, and a reload that contains one is refused
  as a whole.

The job goes build → pack (`artifact.json` is written next to the archive with its SHA-256, commit and job) → deploy:
the archive is read back through its record, uploaded to `upload_dir` over the SSH connection (no SFTP needed), its
SHA-256 is **verified on the target before anything runs**, then `deploy.steps` run in order with `sh -ec`, then `check`
(retried `attempts` times), then `after`. The upload is removed at the end. `DYMPLOY_ARTIFACT` (the remote path),
`DYMPLOY_ARTIFACT_SHA256`, `DYMPLOY_TARGET`, `DYMPLOY_COMMIT`, `DYMPLOY_SHORT_COMMIT`, `DYMPLOY_OWNER`, `DYMPLOY_REPO`,
`DYMPLOY_JOB` and `DYMPLOY_BRANCH` or `DYMPLOY_TAG` are available to the steps (and `DYMPLOY_SECRETS_FILE` when the
definition asks for secrets); database migrations stay on the target.

Exit codes: 4 target missing or denied, 5 a deploy step failed, 6 the health check never passed, 7 an `after` step failed
(the deployment itself is done), 10 finalization failed, 124 `deploy.timeout` ran out. A target that cannot be reached or a damaged upload is an
error of the runner instead. **Without configured recovery**, on failure whatever the steps already did stays as it is and
the log says so. Optional `deploy.steps_undo` supplies explicit recovery after a step or health check fails;
recovery stops on its first failure and never turns the failed job into a success. `deploy.finally.steps`
attempts cleanup/notifications even after failure or cancellation, once connected to the SSH target.
Neither feature changes existing manifests. See [deployment recovery](docs/dymploy-yaml.md#recovery-and-finalization)
for time limits, status variables and maintenance handling.

Nothing outside `internal/runner/docker` knows about containers (`internal/architecture` enforces it, and the same for SSH, which only `cmd` and `internal/deploy/sshremote` know). Tests against a
real engine are tagged: `go test -tags integration ./internal/runner/docker/` (and `./internal/deploy/sshremote/` against a real OpenSSH, see the file's header) (set `DYMPLOY_IT_DIR` under your home when
the engine runs in a VM such as Colima).

## Looking at the jobs

The same binary talks to the running daemon through the control socket (`--socket`, or the one in the configuration):

```sh
dymploy jobs [--state running,queued] [--name github/owner/repo] [--limit 20] [--json]
dymploy job <id>                 # state, exit code, error, timings
dymploy logs <id> [-f]           # -f follows until the job ends (lines: "#" dymploy, "!" stderr)
dymploy abort <id> [--reason "…"]   # stop a job properly (see Stopping): it ends up `aborted`, with the reason
dymploy retry <id>               # a finished job again, as a new job
dymploy watch [--json]           # everything, live: what is waiting or running, then every change and log line
dymploy queue github/owner/repo --ref main [-f]   # start a job by hand (a branch or a tag); -f follows its log
dymploy queue                    # without arguments: what is waiting and running now
dymploy secrets github/owner/repo --ref main [-f]   # change only the target's secrets: no build, no new release
```

`dymploy queue` is not a way around anything. The provider resolves the branch or tag to its commit (a branch is tried before a
tag; `refs/heads/…` and `refs/tags/…` are accepted), the event must pass **the same `policy` as a webhook** (and so a reload applies
to it at once), and the job obeys the queue's rules (`on_busy`, one job per repository, `max_pending`). It works only through
the control socket, never through the public API, and is written to the audit log. For GitHub it needs the App
(`app_id`/`private_key_file`) installed on the repository.

`dymploy secrets` is `dymploy queue` for a different job: it checks out the commit, reads its `dymploy.yaml` and, instead of
building, fetches the target's current secrets and runs the definition's `deploy.secrets_steps` (typically: install the file and
restart the service) and then its `check`; if they fail, `deploy.secrets_undo` runs. It is for when only a secret changed and a
new release would be pointless (and impossible: a commit that is already deployed cannot be deployed again). The same policy,
queue and audit log apply, the job is named after the repository so it never runs together with a deployment of it, and a
definition without `secrets_steps` is refused (exit code 9). See `docs/dymploy-yaml.md`.

The API behind those commands is `GET /v1/jobs`, `/v1/jobs/{id}`, `/v1/jobs/{id}/logs?after=&limit=`, `POST /v1/jobs/{id}/abort`
(body `{"reason": "…"}`) and `/retry`, and the live channel `GET /v1/events` (a WebSocket). The channel starts with a `snapshot` of the jobs waiting or
running and then sends `queued`, `started`, `output` and `finished` messages as JSON. With `?job=<id>&after=<seq>` it first
replays that job's log from the sequence number and follows the job until it finishes, so a client that reconnects loses
nothing: every log line carries its `seq`, and a gap means the client was too slow and can read the log again. The
parameters and input of a job (which can mention repository details) are never part of the API.

Other tools can use the **read-only** part on the public listener, under `/api/v1/`, when `api.token_file` is set
(`Authorization: Bearer <token>`; see `examples/config/`). Without the token file nothing is mounted. Cancelling and retrying
stay on the control socket. Run it behind the reverse proxy over TLS; `api.allowed_origins` lists the web origins allowed to
open the channel from a browser.

## Showing the result next to the commit

With `report_status: true` under `webhooks.github` (it needs the App, with the *Commit statuses: Read and write* permission
accepted by each installation) every run puts its state next to its commit on GitHub: pending while it waits and runs, then the
green tick (it succeeded), the red cross (a step failed, with its exit code) or an error (it could not be completed or was
stopped). It appears as the `dymploy` check. What is published leaves dymploy, so it is deliberately generic: no error text, host
names or stop reasons, only the run number and the exit code. The statuses are sent by the queue's observer, in order and from
their own goroutine, and a GitHub that does not answer costs a warning in the log, never a job; on shutdown the queue lets the
observer finish, so the end of an aborted run is reported too.

## Stopping, without a mess

Nothing is stopped by cutting it off. Stopping a **job** and stopping **dymploy** are different commands on purpose, so a
forgotten argument never stops the wrong thing.

**A job** (`dymploy abort <id> --reason "…"`): a job that has not started never runs. A running one is *asked* to stop: its
runner stops the processes it started (a build container gets SIGTERM and, after `queue.stop_grace`-ish time, SIGKILL; a deploy
command over SSH gets TERM, then KILL), releases what it holds, and only then is the job marked `aborted`. The reason, who asked
and when are written down (`stop_reason`, `stop_requested_at`), the job's log narrates it ("stop requested: …", what the process
printed while cleaning up, "job finished: aborted (…)") and watchers see a `stopping` event before the end. Asking twice is harmless.

**dymploy** (`dymploy stop [--graceful]`, which waits and shows the progress until the daemon is gone; `--no-wait`, `--timeout`):

- `--graceful`: new jobs are refused (webhooks, `dymploy queue` and `retry` get an error that says dymploy is shutting down —
  a webhook gets `503` with `Retry-After`, and the sender's retry is not taken for a duplicate), while **every job, running or
  waiting, runs to the end**; the rest of the API keeps answering so the drain can be watched (`dymploy status` shows
  `draining`). When nothing is left, the daemon goes away.
- without it: everything incoming on the public listener is refused (`503`), every running job is aborted as above with the
  reason, and when all are clean the daemon goes away. Jobs that were still waiting stay `queued` and resume at the next start.
  Stopping while it drains turns the drain into this. If jobs do not stop within `shutdown_timeout` (default 2m) the daemon
  logs it and leaves anyway.
- Signals do the same: SIGTERM and SIGINT stop (a second one leaves at once), SIGQUIT stops gracefully.

A **reload** that takes a repository out of the policy does not just stop accepting it: what that repository still has waiting
or running is stopped the same proper way, with the reason "no longer allowed by the configuration after a reload".

## Development

```sh
go vet ./... && go test -race -cover ./...
```

The architecture rules (which packages may import a provider, the container engine or SSH) are enforced by `internal/architecture`.

## License

MIT

## Durable run events

Version v0.10.0 adds optional server-configured event receivers. Each receiver needs a unique stable
name and a private signing-key file (at least 32 bytes). Configuration changes require a daemon restart.

```yaml
events:
  sinks:
    - name: deployment-history
      url: https://control.example.com/hooks/deployments
      secret_file: /etc/dymploy/events/history.key
      repositories: [github/acme/app]  # exact run names; omitted = all
      types: [queued, started, finished, aborted, interrupted]  # omitted = all
```

Job state changes and their outbox records commit in the **same database transaction**. Receivers are
served independently; a slow/offline receiver never waits inside a deployment. Pending records and
acknowledgements survive daemon restarts. Delivery is ordered per receiver (and therefore per run), with
an HTTP timeout of 10 seconds and exponential retry delays up to one minute. Only 2xx acknowledges an
event; redirects are not followed. Fully acknowledged records are pruned. A failed receiver retains its
backlog, so monitor available disk space. Removing a sink retires its backlog; a new sink starts from the
remaining retained events, not from all historical jobs. No sinks means no event recording overhead.

The JSON body carries event ID/type, job identity and number, state, reference/commit, timestamps,
duration, exit code and per-target outcomes. It excludes command output, raw errors, stop reasons, runner
parameters, secrets and host addresses. Fetch authorized job logs separately when needed.

Headers:

- `X-Dymploy-Event-ID`: stable `<job-id>:<state>`; **deduplicate this ID**. Delivery is at least once:
  a crash after the receiver accepts but before the acknowledgement is stored can replay an event.
- `X-Dymploy-Timestamp`: Unix seconds for this delivery attempt.
- `X-Dymploy-Signature`: `sha256=<hex HMAC-SHA256(key, timestamp + "." + exact-body-bytes)>`.

Receivers should verify the signature with a constant-time comparison, enforce a timestamp tolerance
(e.g. five minutes), and deduplicate before applying side effects. Keep clocks synchronized. These events
complement deployment `finally`: they also cover build failures and jobs interrupted by a daemon crash.

## Detecting changed secrets

After a successful deployment requesting secrets, or a successful `dymploy secrets`, the daemon stores a
per-target **HMAC-SHA-256 fingerprint** and delivery time under `data_dir/state/secrets`. The random private
key is separate from the fingerprint file; neither raw secrets nor fingerprints are exposed through the API.
Preserve this directory when moving the daemon. It is created automatically; existing installations gain
baselines on their next successful delivery, not by assuming that today's source matches a running app.

`dymploy status` reports `changed`, `unavailable` or `checking` for affected destinations. `status --json`
and authenticated `GET /api/v1/status` include the structured comparison and timestamps. Comparisons are
requested asynchronously by status and cached for five minutes, so a slow/broken source does not block
status. A failed source produces `unavailable`, never a false `current`. After changing secrets, run
`dymploy secrets <repository> --ref <deployed-ref>` to apply them; a successful run resets the baseline.

Only destinations with a recorded successful secrets delivery are compared. A successful normal deployment
that no longer requests secrets removes that destination's baseline. Removed targets or sources do not
produce warnings. No secret is applied merely by asking for status.

## Deployment and cache administration (v0.10.0)

- [Multiple destinations and artifact-only retries](docs/dymploy-yaml.md#multiple-deployment-destinations).
- [Optional recovery and finalization](docs/dymploy-yaml.md#recovery-and-finalization).
- [Cache inventory, generations and invalidation](docs/dymploy-yaml.md#cache-compatibility-and-invalidation).

All new manifest fields are optional version-1 extensions. Upgrade the executor before using them;
older executors correctly reject keys they do not understand. No release or production configuration is
changed by adding these capabilities to the source tree.
