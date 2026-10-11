# dymploy.yaml: format reference (version 1)

The file every repository carries in its root (`dymploy.yaml` or `dymploy.yml`, not both). It says how to build,
what to pack and, optionally, where to deploy. Check it with `dymploy validate`, try it with `dymploy try`, and give
your editor the JSON Schema (`dymploy schema`).

## Principles

1. **The file comes from a repository: it is untrusted input.** Unknown keys, anchors and aliases are errors; every size
   is bounded (64 KiB the file, 100 steps, 8 KiB per script); no path may leave the repository or the build; the file
   cannot be a symbolic link.
2. **It names a target, it does not define one.** `deploy.target` is the *name* of a target configured on the dymploy
   server. Machines, users, keys and which repositories may use which target belong to the server's configuration,
   never to the file. Whoever can push to an allowed branch can change the commands, but cannot point them at a machine
   the server has not granted.
3. **No secrets.** The format has nowhere to put them (and `DYMPLOY_*` variables are reserved). Secrets and database
   migrations live on the target.

## Structure

```yaml
version: 1                  # required; the format version this dymploy understands
build:                      # required
  image: …                  # the environment: from (an image) or dockerfile (one of the repository), exactly one
  env: {…}                  # variables for every step
  network: default | none   # none cuts the network of every step
  limits: {cpus, memory, timeout}
  cache: [{name, path}]     # container directories kept between builds
  steps: [{name, run, image, workdir, env}]
artifact:                   # optional; required when there is a deploy
  name: …                   # optional (defaults to the base name of the first path)
  paths: […]
  exclude: […]
deploy:                     # optional; without it the job only builds
  target: production
  timeout: 10m
  steps: [{name, run, env}]
  check: {run, attempts, interval, timeout}
  after: {steps: […]}       # optional
```

A job with **neither `artifact` nor `deploy`** only checks the code (tests, linters): it builds, runs its steps and
succeeds or fails with them.

## build

- **`image`**: `from: debian:13` (pulled) **or** `dockerfile: path` with optional `context` and `args` (built from the
  repository's Dockerfile; the image belongs to the job and is removed afterwards; the Dockerfile and the context cannot
  leave the repository). Not both. `context` and `args` only go with `dockerfile`. If the environment must install
  packages (compilers, git), do it in the Dockerfile: the steps run without privileges.
- **`steps`** (at least one): each is a `run` script (run with `sh -ec`, so it stops at the first failing command) and,
  optionally, a `name` (unique), an `image` (another image for this step only, like the services of a compose file), a
  `workdir` (relative to the repository) and an `env`. Every step runs in its own container with the repository at
  `/work`, as the daemon's own uid:gid and with `HOME=/tmp`.
- **`cache`**: `name` (lowercase letters, digits, `. _ -`, unique) and `path` (absolute inside the container, no `..`).
  Kept per repository between builds. **They must not be inside `/work`** (that is the build tree, and the artifact is read
  from it): use paths such as `/cache/...` and the variables of your tools (`MIX_HOME`, `npm_config_cache`…).
- **`limits`**: `cpus` (> 0, ≤ 64), `memory` (`512m`, `2g`…), `timeout` (default 30m, at most 4h, for the whole build; when
  it runs out the job ends with exit code 124). The server sets defaults and maximums (`runner.params.limits`); asking
  for more is an error.
- **`env`**: valid variable names; those starting with `DYMPLOY_` are reserved; values up to 4 KiB with no NUL. Layers,
  in order: `build.env`, `step.env`, then the `DYMPLOY_*` variables, which always win.

## artifact

- `paths`: **relative to the build directory**, no `..`, not starting with `/` or `-`. Each path is stored in the `tar.gz`
  **under its base name** at the root of the archive (`app/_build/rel/shop` → `shop/…`; `out/book.pdf` → `book.pdf`), so
  the layout is predictable (and a release is installed with `--strip-components=1`). Two paths with the same base name
  are an error.
- `exclude`: patterns (`path.Match` syntax). Without `/` they match the name of a file or directory at any depth
  (`*.log`); with `/` they match the path inside the archive (`shop/log/*`). An excluded directory is left out with
  everything below it.
- Packing rules: fixed order and normalized owners (the same tree gives the same archive), links only if relative and
  inside what is packed, special files refused, a limit on the uncompressed size (4 GiB by default).
- Result, in `<data_dir>/artifacts/<job>/`: `<name>.tar.gz` and **`artifact.json`** (name, file, `sha256`, size, entries,
  provider, owner, repository, commit, ref, job, date). Whatever deploys the archive reads it **through its record**,
  which checks size and sha256. Kept for `artifact_keep_days` (14 by default).
- Exit codes of the job: 2 `dymploy.yaml` missing or invalid (or it asks for more limits than the server allows); 3 the
  build worked but could not be packed; 124 time ran out; in a build step, the step's own code.

## deploy

Runs **on the target**, in order, as the user the target defines, with `sh -ec` and the environment passed through
`env(1)` (no `AcceptEnv` needed). dymploy first uploads the artifact to the target's `upload_dir` and **verifies its
sha256 on the target before running anything**; the upload is removed at the end.

- **`steps`** (at least one): `run`, `name` (unique), `env`. The first step that fails stops the deployment.
- **`check`**: the health check after the steps: `attempts` (1–120, default 5), `interval` (1s–5m, default 3s) and a
  `timeout` per attempt (1s–10m, default 10s). If no attempt succeeds the deployment fails.
- **`after`** (optional): runs only if everything went well. If it fails the job is marked failed, but the deployment is
  already done and is left alone.
- **`timeout`**: for the whole deployment (default 10m, at most 1h).
- **There is no automatic rollback**: if something fails, whatever the steps did stays as it is, and the log says so.
- If a job is stopped in the middle of a deployment, the remote command receives TERM and then KILL. OpenSSH delivers the
  signal to the command's shell, not to its whole tree, so steps should be short and safe to repeat.

### What runs when

| Situation | What happens | Exit code |
|---|---|---|
| The target does not exist or the repository may not use it | The job fails **before building**. | 4 |
| The build fails | The job fails; nothing is deployed. | the step's |
| A `deploy.steps` step fails | It stops; nothing is undone. | 5 |
| `check` fails (attempts used up) | The same. | 6 |
| All went well, `after` fails | The job fails; the deployment stays. | 7 |
| `deploy.timeout` runs out | The command is stopped; nothing is undone. | 124 |
| Target unreachable, damaged upload (sha256 differs), job stopped | An error of the runner (not of the file); a stopped job ends `aborted` with its reason. | — |

## Variables dymploy gives to the commands

`DYMPLOY_COMMIT`, `DYMPLOY_SHORT_COMMIT`, `DYMPLOY_OWNER`, `DYMPLOY_REPO`, `DYMPLOY_BRANCH` or `DYMPLOY_TAG`,
`DYMPLOY_JOB` in build and deploy; in the deploy also `DYMPLOY_TARGET`, `DYMPLOY_ARTIFACT` (path of the uploaded file, on
the target) and `DYMPLOY_ARTIFACT_SHA256`. There are no templates inside the file: use the shell's expansion
(`"$DYMPLOY_ARTIFACT"`).

## Targets (the server's configuration; they can be split across `config.d/*.yaml`)

```yaml
targets:
  production:
    host: 10.0.0.20
    port: 22                      # optional
    user: deploy-shop             # one user per application keeps applications apart
    key_file: /etc/dymploy/targets/production-shop.key   # at most 0600, no passphrase, one key per target
    known_hosts: /etc/dymploy/targets/production.known_hosts   # required: no trust on first use
    upload_dir: dymploy-uploads   # optional; relative to the login directory, or absolute
    allow:                        # which repositories may deploy here (the same rules as `policy`)
      - {provider: github, owner: acme, repositories: [shop], branches: [main]}
```

A repository that asks for a target that does not exist, or that exists but is not granted to it, fails **before**
building. A target whose files do not exist or cannot be used keeps the daemon from starting, and makes a `reload` be
refused as a whole. The connection is only accepted if the server presents the key in `known_hosts`.

## Build security

Steps run in containers **without privileges** (all capabilities dropped, `no-new-privileges`, no access to the container
engine's socket, no devices or ports, bounded processes and memory, no swap, nothing of the server mounted but the
repository, with `.git` read-only, and the caches), with the declared limits, with no secrets of the server in the
environment and with the network the file says. The Dockerfile is the repository's code, and as untrusted as the rest.

## Out of version 1, on purpose

Secrets in the file; several targets in one file; automatic rollback; publishing to object storage; build matrices,
conditionals and including other files; templates. Each would need a version change (`version: 2`) or a backwards
compatible extension.
