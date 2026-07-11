package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/z2z23n0/tooltend/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	command := cli.New(cli.Options{})
	if err := command.ExecuteContext(ctx); err != nil {
		if !cli.IsReported(err) {
			_, _ = fmt.Fprintln(os.Stderr, "Error:", err)
		}
		os.Exit(1)
	}
}
