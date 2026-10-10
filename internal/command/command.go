package command

import (
	"context"
	"io"
	"strconv"

	"github.com/paidwen/paidwen-cli/internal/check"
	"github.com/paidwen/paidwen-cli/internal/env"
	"github.com/paidwen/paidwen-cli/internal/login"
	"github.com/paidwen/paidwen-cli/internal/record"
)

const usage = `Paidwen asks before each action of an AI agent, and keeps the journal.

Usage:
  paidwen check [flags]    ask Paidwen before a tool, from the hook of a coding agent
  paidwen record [flags]   send the result of a tool to the journal, from the hook after the tool
  paidwen login            sign in this machine as your agent, in your browser
  paidwen logout           forget the key of this machine
  paidwen status           show the agent this machine acts as
  paidwen headers          print the HTTP headers of the key, as JSON
  paidwen version          print the version

Run paidwen <command> -h for the flags of a command.
Docs: https://paidwen.com/docs/coding-agents
`

func Run(ctx context.Context, e env.Env, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		write(stderr, usage)
		return 2
	}
	rest := args[1:]
	switch args[0] {
	case "check":
		return check.Run(ctx, e, rest, stdin, stdout, stderr)
	case "record":
		return record.Run(ctx, e, rest, stdin, stdout, stderr)
	case "login":
		return login.Login(ctx, e, rest, stdin, stdout, stderr)
	case "logout":
		return login.Logout(ctx, e, rest, stdin, stdout, stderr)
	case "status":
		return login.Status(ctx, e, rest, stdin, stdout, stderr)
	case "headers":
		return login.Headers(ctx, e, rest, stdin, stdout, stderr)
	case "version":
		write(stdout, "paidwen "+e.Version+"\n")
		return 0
	case "help", "-h", "-help":
		write(stdout, usage)
		return 0
	}
	write(stderr, "paidwen: unknown command "+strconv.Quote(args[0])+"\n\n"+usage)
	return 2
}

func write(w io.Writer, text string) {
	_, _ = io.WriteString(w, text)
}
