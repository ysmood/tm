# Overview

A light weight terminal multiplexer manager. It allows you to use each session just like a normal shell. Unlike tmux or screen, no need to use complex key-bindings to scroll through panes or windows. It's designed to be simple and easy to use for modern terminal applications.

<img width="600" alt="Image" src="https://github.com/user-attachments/assets/d5caa2e7-8698-4a6b-a64f-3452067e2b5c" />

## Installation

```bash
go install github.com/ysmood/tm@latest
```

## Usage

Its cli is by design without any arguments, so you can just type `tm` and hit enter.

```bash
tm
```

It will enter [cli interactive mode](https://github.com/charmbracelet/bubbletea), it will list all the sessions and commands you can use.
tm will use vscode like [fuzzy search](https://github.com/sahilm/fuzzy) to help you filter through them.

### Create a new session

The command is `[new session]`. You just type words like, `new` `ns`, `[n`, `[ns`, etc. and hit enter.

This command will create a new shell session. It will auto generate a default name for the session, you can change it before entering the session.

The auto generated name is based on the current working dir, git repo name, or the current time, etc.

All session info is stored as plain files under a single directory (`~/.tm` by default,
override with the `TM_HOME` environment variable): one small JSON file per session plus its
scrollback log. There is no database, so the session list loads near-instantly and any number
of `tm` windows can read it at once with no locking.

There is also no client-server like tmux: each session runs as its own independent background
process, so there is no shared server that can die and take all your sessions with it.

### Rename a session

The command is `[rename session]`. You just type words like `rename`, `rs`, `[r`, etc. and hit enter.

Pick the session you want to rename — including the one you are currently inside — then edit its
name and hit enter. The session keeps running throughout; only its label changes. Names must be
unique within a namespace.

tm then prints `[tm renamed session <old> → <new>]` above the menu, the same way it notes attaching,
switching and detaching, so the change stays in your scrollback.

### Kill a session

The command is `[kill session]`. You just type words like `kill`, `ks`, `[k`, etc. and hit enter.

Pick the session to kill: its shell is terminated and the session — scrollback and all — is removed
from the list. A session whose background process has already died is simply cleared from the list.

The session you are currently inside is offered too, marked `current` — handy when its shell is
stuck. Killing it ends what is on your terminal along with it and drops you back to the top-level
menu (a `tm` run from inside the session goes down with the shell, and the outer terminal falls
back to the menu the same way).

tm then prints `[tm killed session <name>]` above the menu, so the change stays in your scrollback.

### Clear a session's history

The command is `[clear history]`. You just type words like `clear`, `ch`, `[c`, etc. and hit enter.

Pick the session whose history to wipe — including the one you are currently inside, marked
`current`. The session's log file is truncated, and that log is the only place its history lives:
nothing is buffered in memory. The session keeps running; only its recorded past is gone, so
nothing of it can be replayed on a later attach.

This is handy when a secret — a password, a token — was echoed to the terminal: clearing the
history keeps it from leaking through the session's log file or a later replay.

Re-attaching before the session has produced anything new replays a dim
`[tm history cleared here - might need to press enter for a prompt]` line instead of a blank
screen — the shell prints its prompt only when asked, so right after a wipe there is nothing
else to show.

tm then prints `[tm cleared history of session <name>]` above the menu, so the change stays in
your scrollback.

### Attach to a session

Each session will be listed with its name, just type in the name of the session you want to attach to and hit enter. You will be attached to that session.

Attaching redraws the last window of the session's output — one screenful, read straight from its
log file — so you land on the screen you left, however long the session has been running. The rest
of the history stays in the log file, where `tm` never has to stream it to your terminal.

If you are already in a session, it will switch to the new session instead of nesting sessions.

### Open the menu from inside a session

While you are inside a session, press `Ctrl-\` to pop up the tm menu without
leaving the session. From there you can switch to another session or start a new
one, and pressing `esc` (or `Ctrl-\` again) drops you straight back into the
session you came from, right where you left off.

### Exit a session

Exiting the session's shell itself (typing `exit` or pressing `Ctrl-D`) ends the
session for good — its background process stops and it disappears from the list.
Instead of dropping you out of tm, this returns you to the menu, where you can
pick another session or start a new one. (To keep the session alive, use
`[detach session]` below; to leave tm entirely, `[exit]`.)

### Detach from a session

While you are inside a session, choose the `[detach session]` command from the menu
(`Ctrl-\` opens it). You just type words like `detach`, `ds`, `[d`, `[ds`, etc. and hit enter.
It drops you back to the top-level menu with the session — and every other one — still
running in the background.

The top-level menu, where there is no session to detach from, doesn't offer the command;
leaving tm from there is `[exit]`'s job (also on `esc`, `Ctrl-D`, or `Ctrl-\`). Run `tm`
again to pick a session back up.

### Use namespace

The command is `[use namespace]`. You just type words like `namespace`, `un`, `[n`, etc. and hit enter.

A namespace is a way to group sessions together. When you enter tm, it uses a namespace called `default`,
and you only see sessions under your current namespace. Pick a namespace from the list to switch to it,
or type a name that doesn't exist yet and choose `[new namespace] <name>` to create it and switch to it
in one step.

If you want to see all sessions, you can switch to the `*` namespace.

Set the `TM_NAMESPACE` environment variable to choose the namespace tm opens in
(instead of `default`); new sessions you create then land in it. For example,
`TM_NAMESPACE=work tm` starts in the `work` namespace.

### Drop namespace

The command is `[drop namespace]`. You just type words like `drop`, `dn`, `[d`, etc. and hit enter.
