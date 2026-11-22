package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Ross-cripto/remote-dev/internal/preview"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := preview.Run(ctx, os.Stdout, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "remote-dev: %s\n", err)
		var testExit *preview.TestExitError
		if errors.As(err, &testExit) {
			os.Exit(testExit.Code)
		}
		os.Exit(1)
	}
}
