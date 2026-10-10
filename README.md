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

> The job queue is not built yet: accepted events are logged and dropped until it lands.

## Development

```sh
go vet ./... && go test -race -cover ./...
```

See `AGENTS.md` for the working conventions.

## License

MIT
