![Language](https://img.shields.io/github/languages/count/i5heu/resticprofile-stat-server)
![GitHub top language](https://img.shields.io/github/languages/top/i5heu/resticprofile-stat-server)
![GitHub release (latest by date)](https://img.shields.io/github/v/release/i5heu/resticprofile-stat-server)
![Docker pulls](https://img.shields.io/docker/pulls/i5heu/resticprofile-stat-server)


# resticprofile-stat-server

A lightweight Go server that scans multiple `resticprofile` configurations (one per subdirectory) and serves detailed, **cached backup statistics** over HTTP.

Optimized for low-overhead stats scraping and [Glance](https://github.com/glanceapp/glance) dashboards.

## 🔍 What It Does

For each subfolder inside a configured root (e.g. `/data/bar`, `/data/foo`), this server runs:

1. `resticprofile stats --mode restore-size --json`
2. `resticprofile stats --mode raw-data --json`
3. `resticprofile snapshots --json`

It merges all outputs, enriches them with:

* **Human-readable sizes** (e.g. “4.26 TiB”)
* **Compression ratio & savings** with 2-decimal precision (`"1.02"`, `"2.11%"`)
* **Last snapshot age** (e.g. “15 min ago”)
* **Per-path latest snapshot times**

It parses the structured JSON output, combines it, and exposes the result at [http://0.0.0.0:8080/stats](http://localhost:8080/stats).

## Example Output

```json
[
  {
    "name": "test",
    "restore_bytes": 4685851012530,
    "restore_human": "4.26 TiB",
    "restore_files": 2119631,
    "raw_bytes": 667561804647,
    "raw_human": "621.72 GiB",
    "uncompressed_bytes": 681918411961,
    "uncompressed_human": "635.09 GiB",
    "compression_ratio": 1.021506034668343,
    "compression_ratio_human": "1.02",
    "compression_space_saving": 2.105326247565975,
    "compression_space_saving_human": "2.11%",
    "compression_progress": 100,
    "raw_blob_count": 680045,
    "snapshots": 22,
    "last_snapshot": "15 min ago",
    "paths": [
      {"path":"/data/test","last_snapshot":"15 min ago"},
      {"path":"/data/test/subdir","last_snapshot":"2.3 h ago"}
    ]
  }
]
```


## ⚙️ Configuration

| Env Var                | Default          | Description                          |
| ---------------------- | ---------------- | ------------------------------------ |
| `DATA_ROOT`            | `/data`          | Where to scan for profile dirs       |
| `RESTICPROFILE_BINARY` | `/resticprofile` | Path to the `resticprofile` binary   |
| `CACHE_SECONDS`        | `600`            | How long to cache stats (in seconds) |


## Run It

If you use **docker-composes** see the [docker-compose.yml](docker-compose.yml) file for an example.

### With Docker

```bash
docker run -d \
  --name resticprofile-stat-server \
  -p 8080:8080 \
  -v ./data:/data \
  ghcr.io/i5heu/resticprofile-stat-server:latest
```

### Without Docker
```bash
go build -o stat-server
./stat-server
# or with envs
DATA_ROOT=/backups RESTICPROFILE_BINARY=/usr/local/bin/resticprofile ./stat-server
```

## Notes

* Only one stats run is executed at a time. Concurrent HTTP requests wait on the same result.
* Output is streamed to stdout in real time while running `resticprofile`.
* Safe for Prometheus scraping or ops dashboards.
* Has no authentication or TLS. Use a reverse proxy (e.g. Nginx) for that.
* The server is stateless and can be restarted at any time. It will re-scan the directories.
* The server is designed to be run in a container, e.g. Docker or Kubernetes.


## License

resticprofile-stat-server (c) 2025 Mia Heidenstedt and contributors

SPDX-License-Identifier: AGPL-3.0