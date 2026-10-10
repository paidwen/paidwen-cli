# Paidwen CLI

Paidwen controls what AI agents do. The `paidwen` command brings this control to coding agents, such as Claude Code, through their hooks. Before each tool, the hook of the agent runs `paidwen check`, which asks Paidwen whether the agent may use the tool, then allows or blocks it. After the tool, `paidwen record` sends the result to the journal of Paidwen.

## The command installs in one line

On macOS and Linux:

```sh
curl -fsSL https://paidwen.com/install.sh | sh
```

On Windows:

```powershell
powershell -ExecutionPolicy ByPass -c "irm https://paidwen.com/install.ps1 | iex"
```

The script downloads the archive of your system from the latest GitHub release, checks its SHA-256 against `checksums.txt`, and installs `paidwen` in `~/.local/bin`, or in `%LOCALAPPDATA%\Programs\paidwen` on Windows. It tells you how to add this directory to your `PATH` when it is not there. `PAIDWEN_VERSION` picks another release, and `PAIDWEN_INSTALL_DIR` another directory. They apply to `sh`, after the pipe:

```sh
curl -fsSL https://paidwen.com/install.sh | PAIDWEN_INSTALL_DIR="$HOME/bin" sh
```

With Go:

```sh
go install github.com/paidwen/paidwen-cli/cmd/paidwen@latest
```

Each release carries a build provenance attestation. `gh attestation verify paidwen_linux_amd64.tar.gz -R paidwen/paidwen-cli` checks an archive against it.

## A developer signs in once per machine

```sh
paidwen login
```

`paidwen login` signs this machine in as your agent, in your browser. `paidwen status` shows the agent this machine acts as.

## The hook of Claude Code asks Paidwen before each tool

Add this hook to the settings of Claude Code, for example in `~/.claude/settings.json`:

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "paidwen check -tool /tool_name -params /tool_input -param cwd=/cwd -session /session_id -call /tool_use_id -wait 580s",
            "timeout": 600,
            "onFailure": "block"
          }
        ]
      }
    ]
  }
}
```

`-tool`, `-params`, `-param`, `-session` and `-call` read fields of the JSON that the hook receives, by JSON pointers. `-wait 580s` keeps a wait for a human approval under the `timeout` of 600 seconds. With `"onFailure": "block"`, Claude Code blocks the tool when the command fails or does not answer in time. `paidwen check -h` lists every flag.

The guide for each coding agent is at https://paidwen.com/docs/coding-agents.

The CLI is open source under the Apache 2.0 license.
