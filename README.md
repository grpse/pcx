# pcx

A process manager for a catalog of dev commands, driven by a
[process-compose](https://f1bonacc1.github.io/process-compose/)-style YAML file.
tmux does the supervising — one detached session, one window per process — so
processes keep running when you close the TUI, and each process's real output is
a tmux window you can jump into.

```
go build -o pcx . && cp pcx ~/.local/bin/     # needs tmux
```

## Use

```sh
pcx                      # TUI (creates the session, autostarts non-disabled processes)
pcx up [name...]         # start detached; no names = every process without `disabled: true`
pcx down [-9] [name...]  # SIGTERM, or SIGKILL with -9
pcx restart [name...]
pcx status
pcx logs <name> [-n N]   # dump the tmux scrollback
pcx attach <name> [--pane|--tab|--workspace]
pcx kill                 # kill the session and everything in it
pcx sessions             # running pcx sessions and the config each one runs
pcx -f other.yaml ...    # default: search up for process-compose-x.yaml
pcx -n scratch ...       # a second, independent instance of the same config
pcx down -y ...          # -y skips the confirmation
```

`down`, `restart` and `kill` ask before they act. Without a terminal to ask on
they refuse rather than assuming yes, so scripts have to pass `-y`.

Config lives in `process-compose-x.yaml` (`process-compose.yaml` also works),
found by walking up from the working directory.

### TUI keys

| key | |
|---|---|
| `↑`/`↓`, `j`/`k`, `g`/`G` | move |
| `space`, `enter` | expand/collapse (process trees start collapsed) |
| `H` `L` | scroll command lines toward the start / back to the end |
| `s` | start |
| `x` `X` `r` | stop (TERM) · kill (KILL) · restart — each asks `[y/N]` first |
| `l` | sneak peek: a pane on the right with the highlighted process's live output, which follows the cursor. `l` again closes it |
| `o` | open the process's live output — then pick `p`ane, `t`ab or `w`orkspace |
| `M` | show or hide the executed command tree above peek/output (hidden until you press it) |
| `u` `d` | start / stop everything (`d` asks first) |
| `y` | confirm a pending action; any other key cancels |
| `q` | quit — processes keep running |

On a namespace header, `s`/`x`/`r` apply to every process in it.

Each process row shows status and the CPU and memory of the **whole tree** it
spawned. Expand it to see the command lines (the tail of each argv, `H`/`L` to
scroll), children, their own subtrees, and their pids.

## One session per config

A config file gets exactly one tmux session, named after a hash of its
contents, so opening the same file from any directory reattaches to the
processes already running for it:

```
$ pcx sessions
pcx-a3f35dc2         /Users/you/work/process-compose-x.yaml
```

Editing the file changes that hash, so pcx looks for the session that was
running the same path and renames it — an edit never orphans running processes.

If a second session is running the same file anyway — two terminals racing, a
session left over from an older edit — pcx folds it in: the windows move into
one session, processes keep their pids and output, and the emptied session goes
away. So starting a command that another pcx already opened for this config
takes over that window instead of running a second copy of it.

Set `name:` in the config, or pass `-n <id>`, to name an instance yourself and
run several from one file. That opts out of all of the above — a named instance
is meant to be separate.

## Already running some other way

A process you started by hand or from another tool shows up as `external` with
its real pid, CPU and memory: pcx matches the config's
command against the process table, so the same process is managed from anywhere
instead of being started twice. `up` refuses it, `down`/`restart` act on it.
Only its output is missing — there is no tmux window behind it, so `logs` and
`attach` need it started through pcx.

The match is a substring of the command line, so a command that execs something
else (`npx`, wrapper scripts) runs under different args and will not be found.

## Opening output

`l` is the quick look: one split on the right that re-points itself at whatever
the cursor is on, so moving down the list scrubs through the output of each
process. It needs tmux, closes with `l` again, and closes when the TUI quits.
`M` toggles a second pane above that output listing the commands that starter
actually launched (the live argv tree). It starts hidden; if peek is closed,
`M` opens it first.

`o` in the TUI (or `pcx attach`) shows a process's live output three ways:

- **pane** — splits the current tmux window beside the TUI
- **tab** — opens a new tmux window
- **workspace** — switches the client to the pcx session itself

Pane and tab attach a nested client to a throwaway session *grouped* with ours,
so the process keeps its own window: closing the view never touches the process.
Linking the real window in would look the same until someone pressed `prefix &`
and killed the process with the tab. Outside tmux all three just attach.

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

- One detached tmux session per config (see above), plus a `__pcx` holder
  window so the session survives when everything is stopped.
- Each process is a tmux window with `remain-on-exit on`: when a process dies
  the window stays, keeping its output and exit status, and `respawn-window -k`
  restarts it in place. Windows inherit the environment of the `pcx` you ran
  (minus `TMUX*`); `sudo pcx` still runs the command as `$SUDO_USER`. `PATH`
  prefers user-space bins (nvm, fnm, volta, Homebrew) over `/usr/bin`, so
  `node` / `npx` are the ones from your account, not the system install.
- Stop signals the pane's process group, then any descendant that escaped it.
- `restart: always` is a `while :; do ( cmd ); sleep 1; done` wrapper inside the
  pane, so nothing needs to supervise it from outside.
- CPU/memory come from one `ps` sweep, summed over each pane's descendants.

## Not built

No dependency ordering, health checks, readiness probes, or log files — tmux
scrollback is the log. Attach the real process-compose if you need those.
