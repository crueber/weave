package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/crueber/weave/internal/cli"
	"github.com/crueber/weave/internal/cli/opsbridge"
	"github.com/crueber/weave/internal/config"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/admin"
	"github.com/crueber/weave/internal/domain/auth"
	"github.com/crueber/weave/internal/realtime"
	"github.com/crueber/weave/internal/server"
	"github.com/crueber/weave/internal/version"

	_ "github.com/crueber/weave/internal/all"
)

func main() {
	opsbridge.Install()
	if version.Version == "dev" && admin.Version != "dev" {
		version.Version = admin.Version // images stamp only admin.Version
	}
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		if err := serve(); err != nil {
			fmt.Fprintln(os.Stderr, "weave:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if len(os.Args) > 2 && os.Args[1] == "admin" && os.Args[2] == "mint-key" {
		os.Exit(mintKey(os.Args[3:]))
	}
	if len(os.Args) > 2 && os.Args[1] == "admin" && os.Args[2] == "relink" {
		os.Exit(relink(os.Args[3:]))
	}
	os.Exit(cli.Main(os.Args[1:]))
}

// healthcheck probes the local server's /healthz (for container HEALTHCHECK;
// distroless has no curl). Exit 0 healthy, 1 otherwise.
func healthcheck() int {
	addr := config.Load().Addr
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Fprintln(os.Stderr, "unhealthy:", resp.Status)
		return 1
	}
	return 0
}

func serve() error {
	cfg := config.Load()
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	d, err := db.Open(cfg.DataDir + "/weave.db")
	if err != nil {
		return err
	}
	s := &server.Server{Cfg: cfg, DB: d, Hub: realtime.New(), Svc: map[string]any{"config": cfg}}
	for _, h := range server.Hooks {
		if err := h(s); err != nil {
			return err
		}
	}
	h, err := s.Handler()
	if err != nil {
		return err
	}
	// open SSE streams must end on shutdown, or Shutdown would wait for them forever
	base, stopStreams := context.WithCancel(context.Background())
	srv := &http.Server{Addr: cfg.Addr, Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, BaseContext: func(net.Listener) context.Context { return base }}
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		<-c
		stopStreams()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	fmt.Println("weave listening on", cfg.Addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

// mintKey is the break-glass path for the first API key: it runs on the server host
// (e.g. `docker compose exec weave weave admin mint-key --email you@example.com --role admin`),
// opens the database directly via WEAVE_DATA_DIR and never goes through HTTP.
func mintKey(args []string) int {
	var email, role string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--email":
			i++
			if i < len(args) {
				email = args[i]
			}
		case "--role":
			i++
			if i < len(args) {
				role = args[i]
			}
		default:
			fmt.Fprintln(os.Stderr, "usage: weave admin mint-key --email EMAIL [--role admin|projectOwner|boardUser|worker|guest]")
			return 2
		}
	}
	if email == "" {
		fmt.Fprintln(os.Stderr, "usage: weave admin mint-key --email EMAIL [--role admin|projectOwner|boardUser|worker|guest]")
		return 2
	}
	cfg := config.Load()
	d, err := db.Open(cfg.DataDir + "/weave.db")
	if err != nil {
		fmt.Fprintln(os.Stderr, "weave:", err)
		return 1
	}
	defer d.Close()
	out, err := auth.MintKey(context.Background(), d, email, role)
	if err != nil {
		fmt.Fprintln(os.Stderr, "weave:", err)
		return 1
	}
	fmt.Printf("{\"item\":{\"key\":%q,\"prefix\":%q}}\n", out.Key, out.Prefix)
	return 0
}

// relink is the break-glass path for an account bound to the wrong SSO identity: it clears the
// stored identity (host only, direct database access, like mint-key) so the next SSO login with
// that e-mail address binds the identity it presents. Existing sessions of the user end.
func relink(args []string) int {
	if len(args) != 2 || args[0] != "--email" || args[1] == "" {
		fmt.Fprintln(os.Stderr, "usage: weave admin relink --email EMAIL")
		return 2
	}
	cfg := config.Load()
	d, err := db.Open(cfg.DataDir + "/weave.db")
	if err != nil {
		fmt.Fprintln(os.Stderr, "weave:", err)
		return 1
	}
	defer d.Close()
	if err := auth.RelinkByEmail(context.Background(), d, args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "weave:", err)
		return 1
	}
	fmt.Println(`{"item":{"ok":true}}`)
	return 0
}
