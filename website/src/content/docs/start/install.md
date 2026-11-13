---
title: Install
description: One static binary for macOS, Linux and Windows. No sudo.
sidebar:
  order: 2
---

## macOS / Linux

```sh
curl -fsSL https://raw.githubusercontent.com/muthuishere/tmvp/main/install.sh | sh
```

Installs to `~/.local/bin` (no sudo), verifies the download against the release's `checksums.txt` (sha256), then runs `tmvp version`. Options (env): `TMVP_VERSION=v0.1.0` (default: latest), `TMVP_BIN_DIR=~/bin`.

## Windows

```bat
curl -fsSLO https://raw.githubusercontent.com/muthuishere/tmvp/main/install.cmd && install.cmd
```

Same checks (certutil sha256), installs `tmvp.exe` for the current user.

## From source

```sh
go install github.com/muthuishere/tmvp/cmd/tmvp@latest   # Go 1.26
tmvp version                                             # version, protocol, connector types
```

:::caution[Before the first release]
The installers download from a GitHub release. Until one is published, build from source.
:::

## Then

```sh
tmvp skill install --discover     # optional: the tmvp-discoverer agent skill
tmvp doctor                       # what is installed, what can I connect to
```

External CLIs are needed only for the types that drive them: `aws`, `gcloud`, `az`, `git`, `gh`, `playwright-cli` (web). Your secret manager's CLI (`sec`, `op`, …) runs the `$(…)` commands in config. More: [install details](/reference/install-details/).
