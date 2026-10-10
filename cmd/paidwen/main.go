package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"

	"github.com/paidwen/paidwen-cli/internal/command"
	"github.com/paidwen/paidwen-cli/internal/env"
)

var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	system, err := env.System(built())
	if err != nil {
		fmt.Fprintln(os.Stderr, "paidwen: "+err.Error())
		return 2
	}
	return command.Run(ctx, system, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}

func built() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return version
	}
	return strings.TrimPrefix(info.Main.Version, "v")
}
