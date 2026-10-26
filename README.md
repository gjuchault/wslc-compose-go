# wslc-go

**docker-compose style orchestration for [WSL containers (wslc)](https://learn.microsoft.com/windows/wsl/wsl-container), in Go.**

A Go port of [bacarndiaye/wslc-compose](https://github.com/bacarndiaye/wslc-compose)
(Python). Same commands, same compose-file support, same container labels — shipped as
single static binaries with no runtime to install.

Microsoft's `wslc` CLI manages single containers much like `docker run`, but has no
Compose support yet. `wslc-compose` reads the `compose.yaml` / `docker-compose.yml` you
already use and drives `wslc` for you: networks with DNS aliases, named volumes, bind
mounts (with WSL ⇄ Windows path translation), `depends_on` ordering, project-scoped
naming, config-drift detection and scaling.

## Binaries

| Command | Purpose |
|---|---|
| `wslc-compose` | the standalone compose CLI |
| `wslc` | a wrapper giving the docker/podman UX: `wslc compose up -d`; any other subcommand is forwarded verbatim to the real wslc CLI. It never invokes itself. |

## Install

### Windows (PowerShell)

```powershell
irm https://raw.githubusercontent.com/gjuchault/wslc-compose-go/main/install.ps1 | iex
```

This downloads the latest [release](https://github.com/gjuchault/wslc-compose-go/releases)
into `%LOCALAPPDATA%\Programs\wslc-go`, adds it to your user `PATH` (for `wslc-compose`)
and adds `Set-Alias wslc <that dir>\wslc.exe` to your PowerShell profiles. An alias is
used because the real `C:\Program Files\WSL\wslc.exe` sits on the machine `PATH`, which
always comes first. With the alias, `wslc compose up -d` works, and every other `wslc ...`
command still goes straight to the real CLI.

Options (pass them through a script block):

```powershell
$install = [scriptblock]::Create((irm https://raw.githubusercontent.com/gjuchault/wslc-compose-go/main/install.ps1))
& $install -Version v0.2.1   # pin a release
& $install -NoAlias          # only install wslc-compose on PATH
& $install -Uninstall        # remove binaries, PATH entry and alias
```

If your execution policy blocks profile scripts, the alias won't load. To allow them, run
`Set-ExecutionPolicy -Scope CurrentUser RemoteSigned`.

### Linux / inside a WSL distro

```sh
arch=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
mkdir -p ~/.local/bin
curl -fsSL "https://github.com/gjuchault/wslc-compose-go/releases/latest/download/wslc-go_linux_$arch.tar.gz" \
  | tar -xz -C ~/.local/bin --strip-components=1 --wildcards '*/wslc*'
```

### From source

```sh
go build -o bin/ ./cmd/...
```

Put `bin/` on your `PATH`. The real `wslc` is found via `WSLC_COMPOSE_BIN`, then `PATH`
(`wslc.exe`, `wslc`, skipping the wrapper itself), then `C:\Program Files\WSL\wslc.exe`
(`/mnt/c/...` from WSL).

Cross-compile for a WSL distro from Windows with `GOOS=linux go build -o bin/ ./cmd/...`.

### Releases

Every push to `main` builds Windows and Linux binaries (amd64 and arm64). You can download
them as workflow artifacts. When `cli.Version` in `internal/cli/cli.go` has no release yet,
the push also publishes GitHub release `v<Version>` with `SHA256SUMS`. To ship a new
release, bump that version.

## Quick start

```sh
cd examples/demo
wslc-compose up -d
curl http://localhost:8088
wslc-compose exec app cat /data/log.txt
wslc-compose logs -f app
wslc-compose down -v
```

Add `--dry-run` to any command to print the exact `wslc` invocations instead of running them.

## Commands

```
wslc-compose [-f FILE]... [-p NAME] [--env-file PATH] [--dry-run] [--profile NAME] COMMAND ...

  up        create and start services   (-d, --build, --force-recreate, --scale SVC=N, -t)
  down      stop and remove containers and networks   (-v to remove volumes, -t)
  ps        list project containers     (-q)
  logs      show container logs         (-f, -n TAIL, -t)
  exec      run a command in a service container   (--index, -u, -w, -e, -T)
  start / stop / restart                (-t)
  pull      pull service images
  build     build service images        (--no-cache)
  config    print the resolved configuration
  version   show version information
```

## Compose support

Files: like Docker Compose, `compose.override.yaml` (or `docker-compose.override.yml`) is
loaded on top of the auto-detected file; `-f` can be repeated, and `COMPOSE_FILE` (from the
environment or `.env`) lists files separated by `COMPOSE_PATH_SEPARATOR` (`;` on Windows,
`:` elsewhere). Files merge with the rules of the Compose Specification, `!reset` and
`!override` included; relative paths resolve against the first file's directory.

Supported: `image`, `build` (context, dockerfile, args, target, pull), `command`,
`entrypoint`, `container_name`, `environment`, `env_file`, `ports` (short/long syntax,
ranges, `ip:host:container`, `/udp`), `volumes` (bind, named, tmpfs, Windows paths like
`E:\data:/srv:ro`), `tmpfs`, `networks` (with `aliases`, external), `depends_on`,
`hostname`, `domainname`, `dns*`, `user`, `working_dir`, `labels`, `mem_limit`, `cpus`,
`shm_size`, `ulimits`, `stop_signal`, `gpus`, `stdin_open`, `tty`, `profiles`,
`deploy.replicas`, `deploy.resources.limits`, GPU device reservations.

Accepted with a warning (wslc cannot enforce them): `healthcheck`, `restart`, `cap_add`,
`cap_drop`, `privileged`, `devices`, `extra_hosts`, `sysctls`, `secrets`, `configs`,
`init`, `pid`, `ipc`, `read_only`, `security_opt`, `logging`, and `depends_on`
conditions other than `service_started`.

Interpolation: `$VAR`, `${VAR}`, `${VAR:-default}`, `${VAR-default}`, `${VAR:+alt}`,
`${VAR+alt}`, `${VAR:?error}`, `${VAR?error}`, `$$` — from `.env` (or `--env-file`), with
the process environment taking precedence.

## Naming and state

Containers are named `<project>-<service>-<n>` (or `container_name`), networks and volumes
`<project>_<key>`. All state lives in wslc through these labels, so `wslc-compose` and the
raw `wslc` CLI can be mixed freely:

```
com.wslc-compose.project / .service / .container-number / .config-hash
```

`up` compares the config hash label with the current definition: unchanged running
containers are left alone, stopped ones are started, changed ones are recreated.

> The config hash is computed differently from the Python implementation, so containers
> created by the Python `wslc-compose` are recreated once on the first `up`.
>
> Booleans used as values (environment, labels, build args) keep their YAML text:
> `DEBUG: yes` gives `DEBUG=yes` and `true` gives `true`, as with Docker Compose, where
> the Python implementation sends `True`. Quote them to be explicit. Numbers follow
> YAML 1.1 like the Python implementation: `1e3` and `0o17` stay text, `1.0` stays `1.0`.

See [docs/MIGRATION.md](docs/MIGRATION.md) for migrating an existing hand-rolled wslc setup.

## Layout

```
cmd/wslc-compose     compose CLI entry point
cmd/wslc             `wslc compose ...` wrapper
internal/compose     model, loader, interpolation, wslc argument builders (pure)
internal/engine      subprocess layer over the wslc CLI (retries, path translation)
internal/cli         commands and argument parsing
```

## Development

```sh
go vet ./...
go test ./...
```

## License

MIT — see [LICENSE](LICENSE). Original Python implementation © Bacar Ndiaye.
