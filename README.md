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

The command is `/new-session`. You just type words like, `new` `ns`, `/n`, `/ns`, etc. and hit enter.
`Ctrl-T` runs it straight from the menu, without filtering to it first.

This command will create a new shell session. It will auto generate a default name for the session, you can change it before entering the session.

The auto generated name is based on the current working dir, git repo name, or the current time, etc.

All session info is stored as plain files under a single directory (`~/.tm` by default,
override with the `TM_HOME` environment variable), one directory per session:

```
~/.tm/sessions/<session_id>/meta.json    # name, namespace, shell, cwd, pid
~/.tm/sessions/<session_id>/std.log      # the session's history: every line that scrolled off its screen
~/.tm/sessions/<session_id>/daemon.log   # the session's background process's own diagnostics
~/.tm/sessions/<session_id>/daemon.lock  # locked while the background process runs
```

A session whose background process died without cleaning up (a crash, `kill -9`, a reboot) is
removed the next time the menu opens. tm knows it is dead because its `daemon.lock` is no longer
locked — the OS drops the lock however the process ends — so this holds even when the old process
id has since been reused by an unrelated process.

Everything a session owns lives in its own directory, so a session is one self-contained thing
to look at, copy, or delete. There is no database, so the session list loads near-instantly and
any number of `tm` windows can read it at once with no locking. (The session's unix socket is
the exception: it lives under `/tmp`, since socket paths have a short OS length limit.)

There is also no client-server like tmux: each session runs as its own independent background
process, so there is no shared server that can die and take all your sessions with it.

The socket directory defaults to `/tmp/tm-<uid>` and can be moved with the `TM_RUNTIME`
environment variable (say, onto a per-user runtime tmpfs). Every `tm` that talks to a session
must resolve it to the same path, so set it in a shared profile rather than per command.

### Rename a session

The command is `/rename-session`. You just type words like `rename`, `rs`, `/r`, etc. and hit enter.

Pick the session you want to rename — including the one you are currently inside — then edit its
name and hit enter. The session keeps running throughout; only its label changes. Names must be
unique within a namespace.

tm then prints `[tm renamed session <old> → <new>]` above the menu, the same way it notes attaching,
switching and detaching, so the change stays in your scrollback.

### Kill a session

The command is `/kill-session`. You just type words like `kill`, `ks`, `/k`, etc. and hit enter.

Pick the session to kill: its shell is terminated and the session — scrollback and all — is removed
from the list. A session whose background process has already died is simply cleared from the list.

The session you are currently inside is offered too, marked `current` — handy when its shell is
stuck. Killing it ends what is on your terminal along with it and drops you back to the top-level
menu (a `tm` run from inside the session goes down with the shell, and the outer terminal falls
back to the menu the same way).

tm then prints `[tm killed session <name>]` above the menu, so the change stays in your scrollback.

### Clear a session's history

The command is `/clear-history`. You just type words like `clear`, `ch`, `/c`, etc. and hit enter.

Pick the session whose history to wipe — including the one you are currently inside, marked
`current`. The session's log file is truncated and the copy of its screen tm keeps is blanked, so
nothing of its past is left on disk or in memory. The session keeps running; only its recorded past
is gone, so nothing of it can be paged or replayed on a later attach.

This is handy when a secret — a password, a token — was echoed to the terminal: clearing the
history keeps it from leaking through the session's log file or a later replay.

Re-attaching before the session has produced anything new replays a dim
`[tm history cleared here - might need to press enter for a prompt]` line instead of a blank
screen — the shell prints its prompt only when asked, so right after a wipe there is nothing
else to show.

tm then prints `[tm cleared history of session <name>]` above the menu, so the change stays in
your scrollback.

### View a session's history

The command is `/history`. You just type words like `history`, `/hi`, etc. and hit enter.

Pick the session whose recorded output you want to read — including the one you are currently
inside, marked `current`. tm opens it in `less -R`, the same way `git show` opens its output in a
pager, so the whole scrollback is scrollable and searchable and its colors are preserved: first the
session's log file, then what its screen shows right now. The session keeps running and nothing on
your screen is disturbed; quit the pager (`q`) to return to the menu.

History is what your terminal's own scrollback would hold. tm keeps a copy of each session's
screen — a small terminal emulator, fed the same output your terminal gets — and a line is
recorded only when it scrolls off the top of that screen, or when the screen is cleared (`clear`
and Ctrl-L keep what was on screen, as tmux does). A program that redraws in place — a shell
prompt, a progress bar, an inline UI like Claude Code repainting itself — changes the screen but
adds nothing to the history, and a full-screen app (vim, htop, ...) never does. Like a reflowing
terminal, resizing re-wraps long lines to the new width, and a wrapped line is recorded as the one
line it is. Hyperlinks (OSC 8) are kept with their text.

A session's log is capped at 64 MiB, so a session that prints forever can't fill the disk: once the
log grows past that, its oldest lines are dropped, keeping the newest 32 MiB.

### Attach to a session

Each session will be listed with its name, just type in the name of the session you want to attach to and hit enter. You will be attached to that session.

Attaching redraws the session's screen exactly as it stands — text, colors, links, cursor position — with
a screenful of its history above it in your terminal's scrollback, so you land on the screen you
left, however long the session has been running, and a program in the middle of redrawing itself
carries on from the right place. The rest of the history stays in the log file, where `tm` never
has to stream it to your terminal.

If the session is inside a full-screen app (lazygit, vim, htop, ...), attaching hands the terminal
back to it: the app's screen and modes are restored and the app itself is prompted to repaint, so
you land back in its UI rather than on a broken mix of history and stale paint.

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
`/detach-session` below; to leave tm entirely, `/exit`.)

### Detach from a session

While you are inside a session, choose the `/detach-session` command from the menu
(`Ctrl-\` opens it). You just type words like `detach`, `ds`, `/d`, `/ds`, etc. and hit enter.
It drops you back to the top-level menu with the session — and every other one — still
running in the background.

The top-level menu, where there is no session to detach from, doesn't offer the command;
leaving tm from there is `/exit`'s job (also on `esc`, `Ctrl-D`, or `Ctrl-\`). Run `tm`
again to pick a session back up.

### Use namespace

The command is `/use-namespace`. You just type words like `namespace`, `un`, `/u`, etc. and hit enter.
`Ctrl-G` runs it straight from the menu.

A namespace is a way to group sessions together. When you enter tm, it uses a namespace called `default`,
and you only see sessions under your current namespace. Pick a namespace from the list to switch to it,
or type a name that doesn't exist yet and choose `/new-namespace <name>` to create it and switch to it
in one step.

If you want to see all sessions, you can switch to the `*` namespace.

Set the `TM_NAMESPACE` environment variable to choose the namespace tm opens in
(instead of `default`); new sessions you create then land in it. For example,
`TM_NAMESPACE=work tm` starts in the `work` namespace.

### Drop namespace

The command is `/drop-namespace`. You just type words like `drop`, `dn`, `/dn`, etc. and hit enter.

Pick the namespace to remove. Its sessions are not touched — they keep running and move to the
`default` namespace, so nothing is lost by dropping the group they were grouped under. The
`default` namespace itself can't be dropped, so it is not offered.

### Show the help

The command is `/help`. You just type words like `h`, `help`, `/h`, etc. and hit enter.

It lists every key and every command with a one-line description — the same reference as this
section of the README, without leaving the menu. Any key returns to the menu.
