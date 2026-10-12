# pcx

A process manager for a catalog of dev commands, driven by a
[process-compose](https://f1bonacc1.github.io/process-compose/)-style YAML file.
Commands run in the background and keep running when you close the TUI.
[Herdr](https://herdr.dev) opens a live tail only when you request a pane, tab,
or workspace. Starting processes creates no Herdr spaces or tabs.

```sh
bash ./install.sh     # builds pcx into ~/.local/bin; needs Herdr 0.9.3 or later
```

Using `bash` works even when a downloaded copy of `install.sh` is not marked
executable. To run it directly instead:

```sh
chmod +x install.sh
./install.sh
```

### macOS security

The installer removes Gatekeeper's quarantine attribute from the `pcx` binary
that it builds locally. If macOS still says that Apple cannot verify the
developer, run:

```sh
xattr -d com.apple.quarantine ~/.local/bin/pcx
```

Alternatively, try to run `pcx`, then open **System Settings → Privacy &
Security** and choose **Open Anyway**. Only bypass Gatekeeper when you trust the
source you built.

## Use

```sh
pcx                      # TUI (autostarts non-disabled processes in the background)
pcx up [name...]         # start detached; no names = every process without `disabled: true`
pcx down [-9] [name...]  # SIGTERM, or SIGKILL with -9
pcx restart [name...]
pcx status
pcx logs <name> [-n N]   # dump the tail of captured process output
pcx attach <name> [--pane|--tab|--workspace]
pcx kill                 # kill managed processes and forget the session
pcx sessions             # pcx sessions and their config files
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
| `M` | show or hide the executed command tree below peek/output (hidden until you press it) |
| `D` | show or hide internal process-manager diagnostics in a pane below the TUI |
| `u` `d` | start / stop everything (`d` asks first) |
| `y` | confirm a pending action; any other key cancels |
| `q` | quit — processes keep running |

On a namespace header, `s`/`x`/`r` apply to every process in it.

Each process row shows status and the CPU and memory of the **whole tree** it
spawned. Expand it to see the command lines (the tail of each argv, `H`/`L` to
scroll), children, their own subtrees, and their pids.

## One process session per config

A config gets a local process session named after a hash of its contents.
Opening the same file from any directory reconnects to its running commands.
Process sessions do not create Herdr workspaces.

```
$ pcx sessions
pcx-a3f35dc2         /Users/you/work/process-compose-x.yaml
```

Editing the file changes its hash. pcx adopts the processes registered for the
same config path, preserving their PIDs and output. A file lock prevents two
clients from starting the same managed command at once.

Set `name:` in the config, or pass `-n <id>`, to name an instance yourself and
run several from one file. Named instances opt out of adoption across config
edits.

## Already running some other way

A process you started by hand or from another tool shows up as `external` with
its real pid, CPU and memory: pcx matches the config's
command against the process table, so the same process is managed from anywhere
instead of being started twice. `up` refuses it, `down`/`restart` act on it.
Its output has not been captured, so `logs` and `attach` need it started through
pcx.

The match requires exact program arguments or an exact shell `-c` command.
A command that execs something
else (`npx`, wrapper scripts) runs under different args and will not be found.

Global child commands are also recognized when their exact command matches and
`~/.pcx/pids/<pid>.lock` exists. Store the actual PID as plain text in the
lock file. These processes appear as `running`, even when launched by another
command. Sending any signal through pcx uses `kill -9` on the stored PID and
removes its lock file. Invalid locks and PIDs absent from the process table are
ignored.

## Opening output

`l` is the quick look: one split on the right that re-points itself at whatever
the cursor is on, so moving down the list scrubs through the output of each
process. It needs pcx running inside Herdr, closes with `l` again, and closes when
the TUI quits.
`M` toggles a second pane below that output listing the commands that starter
actually launched (the live argv tree). It starts hidden; if peek is closed,
`M` opens it first.

`D` opens a diagnostic pane below the TUI. It follows the internal manager log,
including queued operations, execution order, duration, summarized results,
errors, and recovered panics. Press `D` again to close it; quitting pcx also
closes the pane.

`o` in the TUI (or `pcx attach`) shows a process's live output three ways:

- **pane** — splits the calling Herdr tab beside the TUI with a live tail
- **tab** — opens a live tail in one new tab of the calling workspace
- **workspace** — creates one new workspace containing the live tail

The view starts automatically, shows the last 100 lines of command output,
and follows new output. Quiet commands show “No output yet” until they write
something. The shell prompt and pcx's launcher are hidden.

Closing any output view leaves its process running. Restarting a process keeps
its output file, so an open tail continues following the replacement. These
views display output; they do not send keyboard input to the managed command.
`M` puts the command tree below the output.

Inside Herdr, views use the current server and the calling pane's IDs. Outside
Herdr, opening output starts a named `pcx` server, creates one output workspace,
and attaches to it. Set `PCX_HERDR_SESSION=<name>` to choose the output server.
Process management and captured logs work without Herdr running or installed.

## Config

A subset of the process-compose schema, so existing files mostly just work:

```yaml
name: xilo-workspace          # process session name (default: hash of config contents)

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

- A detached pcx runner hosts each command in a pseudoterminal, preserving
  terminal output behavior and capturing stdout and stderr in a private log.
  It records the process PID and exit status independently of Herdr views.
- Commands inherit the invoking pcx's environment, with YAML overrides and
  stale multiplexer context removed. Launch specifications, exit state, and
  output logs live under `${XDG_STATE_HOME:-~/.local/state}/pcx`. Launch
  specifications are removed after the runner reads them. A file lock
  serializes process management across clients.
- pcx detects the invoking shell from its parent process chain (Bash, Zsh, Fish,
  Sh, Dash, Ash, or Ksh), then runs commands in that shell's login environment;
  `$SHELL` and `/bin/sh` are fallbacks. `sudo pcx` still runs commands as
  `$SUDO_USER`. `PATH` is the selected shell's `PATH` with `~/.local/bin` and
  `~/bin` first, so user-space commands win over `/usr/bin`.
- Stop signals the managed process and its descendants. Restart sends TERM,
  waits briefly, and uses KILL if needed before launching the replacement.
- Restart policies use the selected shell's own loop syntax inside the runner,
  so they continue working with the TUI closed.
- CPU/memory come from a `ps` sweep, summed over each process's descendants.
- `pcx kill` signals the config's managed processes and removes its process
  session. Output views and unrelated Herdr workspaces are left open.

## Not built

No dependency ordering, health checks, readiness probes, or log rotation.
Use the real process-compose if you need those.
