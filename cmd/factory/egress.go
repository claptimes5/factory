package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/claptimes5/factory/internal/egress"
)

// egressProxy runs the jobs' egress proxy, inside its container, with the
// rules the daemon passed in FACTORY_EGRESS_ALLOW and _PRIVATE.
func egressProxy(args []string) error {
	fs := flag.NewFlagSet("egress-proxy", flag.ContinueOnError)
	listen := fs.String("listen", ":3128", "address to listen on")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var rules []egress.Rule
	for env, private := range map[string]bool{"FACTORY_EGRESS_ALLOW": false, "FACTORY_EGRESS_PRIVATE": true} {
		for _, s := range strings.Split(os.Getenv(env), ",") {
			if strings.TrimSpace(s) == "" {
				continue
			}
			r, err := egress.ParseRule(s)
			if err != nil {
				return err
			}
			r.Private = private
			rules = append(rules, r)
		}
	}
	if len(rules) == 0 {
		return fmt.Errorf("egress-proxy: no hosts to allow")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	return egress.Serve(ctx, *listen, &egress.Proxy{Rules: rules, Log: log})
}
