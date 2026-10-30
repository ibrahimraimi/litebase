package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ibrahimraimi/litebase/internal/api"
	"github.com/ibrahimraimi/litebase/internal/auth"
	"github.com/ibrahimraimi/litebase/internal/control"
	"github.com/ibrahimraimi/litebase/internal/engine"
	"github.com/ibrahimraimi/litebase/internal/manager"
	"github.com/spf13/cobra"
)

type serveOptions struct {
	addr    string
	dataDir string
	tlsCert string
	tlsKey  string
	isDev   bool
}

func newServeCommand() *cobra.Command {
	opts := &serveOptions{}
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the Litebase HTTP API server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServerProcess(cmd, *opts)
		},
	}
	cmd.Flags().StringVar(&opts.addr, "addr", "127.0.0.1:8080", "Listen address (host:port)")
	cmd.Flags().StringVar(&opts.dataDir, "data-dir", "./litebase-data", "Data directory for control.db and databases")
	cmd.Flags().StringVar(&opts.tlsCert, "tls-cert", "", "Path to TLS certificate file")
	cmd.Flags().StringVar(&opts.tlsKey, "tls-key", "", "Path to TLS private key file")
	return cmd
}

func newDevCommand(flags *RuntimeFlags) *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:   "dev",
		Short: "Run an embedded local development server on 127.0.0.1",
		RunE: func(cmd *cobra.Command, _ []string) error {
			devDataDir := filepath.Join(flags.WorkDir, ".litebase", "dev-data")
			return runServerProcess(cmd, serveOptions{
				addr:    addr,
				dataDir: devDataDir,
				isDev:   true,
			})
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:8080", "Local listen address")
	return cmd
}

func runServerProcess(cmd *cobra.Command, opts serveOptions) error {
	ctx, stopSignals := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	store, err := control.OpenStore(ctx, filepath.Join(opts.dataDir, "control.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	mgr, err := manager.NewManager(manager.Config{
		DataDir: opts.dataDir,
		Store:   store,
	})
	if err != nil {
		return err
	}
	tokenPlaintext, err := bootstrapAdminToken(ctx, store)
	if err != nil {
		return err
	}
	if opts.isDev {
		_, _ = mgr.CreateDatabase(ctx, "main", engine.DurabilityNormal)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Litebase listening on %s (data-dir: %s)\n", opts.addr, opts.dataDir)
	if tokenPlaintext != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "Bootstrap admin token (shown once): %s\n", tokenPlaintext)
	}

	logger := slog.New(slog.NewJSONHandler(cmd.ErrOrStderr(), nil))
	apiServer := api.NewServer(mgr, api.ServerConfig{Logger: logger})
	httpServer := &http.Server{
		Addr:              opts.addr,
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return listenAndShutdown(ctx, httpServer, apiServer, opts)
}

func bootstrapAdminToken(ctx context.Context, store *control.Store) (string, error) {
	envToken := strings.TrimSpace(os.Getenv(BootstrapTokenEnvVar))
	if envToken != "" {
		if !strings.HasPrefix(envToken, auth.TokenPrefixHeader) || len(envToken) <= 12 {
			return "", fmt.Errorf("invalid %s: must start with %s and be at least 13 characters", BootstrapTokenEnvVar, auth.TokenPrefixHeader)
		}
		generated := auth.GeneratedToken{
			Plaintext: envToken,
			Prefix:    envToken[:12],
			Hash:      auth.HashToken(envToken),
		}
		_, err := store.SaveGeneratedToken(ctx, generated, control.CreateTokenParams{
			Scopes: []auth.Scope{auth.ScopeAdmin},
		})
		if err != nil && !errors.Is(err, control.ErrAlreadyExists) {
			return "", err
		}
		return "", nil
	}
	hasAdmin, err := store.HasAdminToken(ctx)
	if err != nil {
		return "", err
	}
	if hasAdmin {
		return "", nil
	}
	_, plaintext, err := store.CreateToken(ctx, control.CreateTokenParams{
		Scopes: []auth.Scope{auth.ScopeAdmin},
	})
	return plaintext, err
}

func listenAndShutdown(
	ctx context.Context,
	httpServer *http.Server,
	apiServer *api.Server,
	opts serveOptions,
) error {
	serveErrCh := make(chan error, 1)
	go func() {
		if opts.tlsCert != "" && opts.tlsKey != "" {
			serveErrCh <- httpServer.ListenAndServeTLS(opts.tlsCert, opts.tlsKey)
			return
		}
		serveErrCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-serveErrCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		return apiServer.Shutdown(shutdownCtx)
	}
}
