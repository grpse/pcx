# pcx

A process manager for a catalog of dev commands, driven by a
[process-compose](https://f1bonacc1.github.io/process-compose/)-style YAML file.
tmux does the supervising — one detached session, one window per process — so
processes keep running when you close the TUI, and each process's real output is
a tmux window you can jump into.

```
go build -o pcx . && cp pcx /usr/local/bin/     # needs tmux
```

## Use

```sh
pcx                      # TUI (creates the session, autostarts non-disabled processes)
pcx up [name...]         # start detached; no names = every process without `disabled: true`
pcx down [-9] [name...]  # SIGTERM, or SIGKILL with -9
pcx restart [name...]
pcx status
pcx logs <name> [-n N]   # dump the tmux scrollback
pcx attach <name>        # jump to the process's tmux window
pcx kill                 # kill the whole tmux session
pcx -f other.yaml ...    # default: search up for process-compose-x.yaml
```

Config lives in `process-compose-x.yaml` (`process-compose.yaml` also works),
found by walking up from the working directory.

### TUI keys

| key | |
|---|---|
| `↑`/`↓`, `j`/`k`, `g`/`G` | move |
| `space`, `enter` | expand/collapse (process trees start collapsed) |
| `s` `x` `X` `r` | start · stop (TERM) · kill (KILL) · restart |
| `o` | open the process's live output in tmux |
| `u` `d` | start/stop everything |
| `q` | quit — processes keep running |

On a namespace header, `s`/`x`/`r` apply to every process in it.

Each process row shows status and the CPU and memory of the **whole tree** it
spawned. Expand it to see the children, their own subtrees, and their pids.

## Config

A subset of the process-compose schema, so existing files mostly just work:

```yaml
name: xilo-workspace          # tmux session name (default: config's directory)

vars:
  WORKING_DIR: "."
  XILO_API: "../xilo-api"

processes:
  identity-start:
    command: "npx nx run identity:start"
    working_dir: '{{or "${WORKING_DIR}" .WORKING_DIR}}'
    description: "identity:start"
    namespace: xilo-workspace
    disabled: true            # in the catalog, not autostarted
    environment: ["PORT=3000"]
    availability:
      restart: always         # always | on_failure | no (default)
```

`working_dir` resolves `${ENV}` first and then the Go template against `vars`,
which is what makes `{{or "${WORKING_DIR}" .WORKING_DIR}}` pick the environment
override when set and the `vars` default otherwise. Relative paths resolve
against the config file's directory.

See `examples/` for a full catalog and a demo you can run.

## How it works

- One detached tmux session per config, plus a `__pcx` holder window so the
  session survives when everything is stopped.
- Each process is a tmux window with `remain-on-exit on`: when a process dies
  the window stays, keeping its output and exit status, and `respawn-window -k`
  restarts it in place.
- Stop signals the pane's process group, then any descendant that escaped it.
- `restart: always` is a `while :; do ( cmd ); sleep 1; done` wrapper inside the
  pane, so nothing needs to supervise it from outside.
- CPU/memory come from one `ps` sweep, summed over each pane's descendants.

## Not built

No dependency ordering, health checks, readiness probes, or log files — tmux
scrollback is the log. Attach the real process-compose if you need those.
