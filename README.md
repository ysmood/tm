# Overview

A light weight terminal multiplexer manager. It allows you to use each session just like a normal shell. Unlike tmux or screen, no need to use complex key-bindings to scroll through panes or windows. It's designed to be simple and easy to use for modern terminal applications.

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

### Attach to a session

Each session will be listed with its name, just type in the name of the session you want to attach to and hit enter. You will be attached to that session.

When you attach to a session, you will be prompted to choose how much log history you want to see, such as all history, one page, or a specific number of lines.

If you are already in a session, it will switch to the new session instead of nesting sessions.

### Detach from a session

The command is `[detach session]`. You just type words like `detach`, `ds`, `[d`, `[ds`, etc. and hit enter.

### New namespace

The command is `[new namespace]`. You just type words like `namespace`, `nn`, `[n`, etc. and hit enter.

Namespace is a way to group sessions together. When you enter tm, it will use a namespace called `default`.
You will only see sessions under your current namespace. You can create a new namespace and switch to it.

### Use namespace

The command is `[use namespace]`. You just type words like `namespace`, `un`, `[n`, etc. and hit enter.

If you want to see all sessions, you can switch to the `*` namespace.

### Drop namespace

The command is `[drop namespace]`. You just type words like `drop`, `dn`, `[d`, etc. and hit enter.
