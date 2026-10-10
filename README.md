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

## The hook of a coding agent asks Paidwen before each tool

Save this template as `~/.paidwen/hook-answer.tmpl`. It prints the refusal and its reason as the JSON that Codex and Claude Code read, and nothing when the tool may run:

```text
{{ if not .Allowed }}{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":{{ json .Reason }}}}{{ end }}
```

Then add this hook to `~/.codex/hooks.json`, and trust it once with `/hooks` in Codex:

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": ".*",
        "hooks": [
          {
            "type": "command",
            "command": "paidwen check -tool /tool_name -params /tool_input -param cwd=/cwd -session /session_id -call /tool_use_id -wait 0s -output-file ~/.paidwen/hook-answer.tmpl",
            "timeout": 60
          }
        ]
      }
    ]
  }
}
```

`-tool`, `-params`, `-param`, `-session` and `-call` read fields of the JSON that the hook receives, by JSON pointers. `-wait 0s` answers at once when a rule asks for a human approval, and the same command runs once someone approves it. Claude Code takes the same line in `~/.claude/settings.json`, with `-wait 580s`, `"timeout": 600` and `"onFailure": "block"`, since it waits for its hooks. Claude Code runs its hooks in a Bash shell, Git Bash on Windows, which turns an argument that starts with `/` into a Windows path: start its line with `MSYS_NO_PATHCONV=1 paidwen check`, which keeps the pointers as they are. `paidwen check -h` lists every flag.

The guide for each coding agent is at https://paidwen.com/docs/coding-agents.

The CLI is open source under the Apache 2.0 license.
