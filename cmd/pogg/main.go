package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/cheetahbyte/pogg/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		structured := false
		for _, arg := range os.Args[1:] {
			if arg == "--json" || arg == "--json=true" {
				structured = true
			}
			if arg == "--json=false" {
				structured = false
			}
		}
		var reported cli.ReportedError
		if !errors.As(err, &reported) {
			if structured {
				json.NewEncoder(os.Stderr).Encode(map[string]string{"error": err.Error()})
			} else {
				fmt.Fprintln(os.Stderr, "pogg:", strings.TrimSpace(err.Error()))
			}
		}
		os.Exit(1)
	}
}
