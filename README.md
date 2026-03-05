# git-server-metrics

`git-server-metrics` is a standalone Go service that collects repository metrics from local Git repositories and exposes them in Prometheus format.

## Purpose

The service periodically scans configured bare/non-bare Git repositories and exports metrics about:

- git object/pack stats
- filesystem shape under `objects/`
- per-collector collection timestamps

All metrics are exposed on `GET /metrics`.

## Build

```bash
cd git-server-metrics
go build -o git-server-metrics .
```

## Configuration

Default config file is `config.yaml` in the working directory.
You can override it with the `CONFIG` environment variable.

Example:

```yaml
listen_addr: ":9108"

git_bin: git
pool_size: 2
command_timeout: 20s

scrape_interval: 30s

repos:
  - name: test-repo
    path: /path/to/repos/test-repo.git
  - path: /path/to/another/repo.git
```

Notes:

- `name` is optional; if omitted, it is derived from repo path basename.
- repository names are sanitized before being exported as `repo_name` label values.
- `scrape_interval` controls how often all collectors run.

## Run

```bash
CONFIG=config.yaml ./git-server-metrics
```

Then check:

```bash
curl -s http://127.0.0.1:9108/metrics | grep '^plugins_git_repo_metrics_'
```

## Prometheus Scrape Example

```yaml
scrape_configs:
  - job_name: git-server-metrics
    static_configs:
      - targets: ["127.0.0.1:9108"]
```

## Exposed Metric Prefix

All exported series use this prefix:

- `plugins_git_repo_metrics_*`
