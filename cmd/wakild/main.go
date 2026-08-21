package main

// cmd/wakild/main.go: the wakild daemon entry point (card #148 P2d).
//
// wakild runs the Connect server over a Unix socket. It opens a fail-closed
// SQLite event store (or --ephemeral for in-memory), builds the session host,
// and serves RPCs until SIGTERM/SIGINT. The TUI connects via --daemon.
//
// Usage:
//
//	wakild [--socket <path>] [--ephemeral] [--shutdown-timeout <dur>]
//
// The daemon reads the same wakil.yaml config as the TUI for backend/model
// credentials. It derives the workspace ID from the config's working directory.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/crypto"
	"github.com/treeol/wakil/internal/scrub"
	"github.com/treeol/wakil/internal/wiring"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "wakild:", err)
		os.Exit(1)
	}
}

// daemonFlags holds the parsed command-line flags.
type daemonFlags struct {
	socketPath        string
	ephemeral         bool
	shutdownTimeout   time.Duration
	httpAddr          string // TCP address for web UI (e.g. "127.0.0.1:8791"; empty = disabled)
	tlsCertFile       string // PEM-encoded TLS certificate file (enables TLS on TCP listener)
	tlsKeyFile        string // PEM-encoded TLS private key file
	allowedOrigins    string // comma-separated list of allowed Origin URLs for CSRF protection (production)
	masterKeyFile     string // path to master key file for envelope encryption (P4g)
	generateMasterKey string // generate a new master key and write to this path, then exit (P4g)
	scrubLevel        string // scrubbing level: off|standard|aggressive (default: standard)
}

func parseFlags(args []string) (daemonFlags, error) {
	f := daemonFlags{
		socketPath:      defaultSocketPath(),
		shutdownTimeout: 10 * time.Second,
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--socket", "-socket":
			i++
			if i >= len(args) {
				return f, fmt.Errorf("--socket requires a path")
			}
			f.socketPath = args[i]
		case "--ephemeral", "-ephemeral":
			f.ephemeral = true
		case "--shutdown-timeout", "-shutdown-timeout":
			i++
			if i >= len(args) {
				return f, fmt.Errorf("--shutdown-timeout requires a duration")
			}
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return f, fmt.Errorf("--shutdown-timeout: %w", err)
			}
			f.shutdownTimeout = d
		case "--http-addr", "-http-addr":
			i++
			if i >= len(args) {
				return f, fmt.Errorf("--http-addr requires an address")
			}
			f.httpAddr = args[i]
		case "--tls-cert", "-tls-cert":
			i++
			if i >= len(args) {
				return f, fmt.Errorf("--tls-cert requires a path")
			}
			f.tlsCertFile = args[i]
		case "--tls-key", "-tls-key":
			i++
			if i >= len(args) {
				return f, fmt.Errorf("--tls-key requires a path")
			}
			f.tlsKeyFile = args[i]
		case "--allowed-origins", "-allowed-origins":
			i++
			if i >= len(args) {
				return f, fmt.Errorf("--allowed-origins requires a value")
			}
			f.allowedOrigins = args[i]
		case "--master-key-file", "-master-key-file":
			i++
			if i >= len(args) {
				return f, fmt.Errorf("--master-key-file requires a path")
			}
			f.masterKeyFile = args[i]
		case "--generate-master-key", "-generate-master-key":
			i++
			if i >= len(args) {
				return f, fmt.Errorf("--generate-master-key requires a path")
			}
			f.generateMasterKey = args[i]
		case "--scrub-level", "-scrub-level":
			i++
			if i >= len(args) {
				return f, fmt.Errorf("--scrub-level requires a value")
			}
			f.scrubLevel = args[i]
		case "--help", "-help", "-h":
			fmt.Fprint(os.Stderr, usage)
			os.Exit(0)
		default:
			if len(args[i]) > 0 && args[i][0] == '-' {
				return f, fmt.Errorf("unknown flag: %s", args[i])
			}
			return f, fmt.Errorf("unexpected argument: %s (wakild takes flags only)", args[i])
		}
	}
	return f, nil
}

const usage = `wakild — wakil daemon (card #148 P2d)

Usage:
  wakild [flags]

Flags:
  --socket <path>            Unix socket path (default: $XDG_RUNTIME_DIR/wakild.sock)
  --http-addr <addr>         TCP address for web UI (e.g. 127.0.0.1:8791; empty = disabled)
  --tls-cert <path>          PEM TLS certificate file (enables HTTPS on TCP listener)
  --tls-key <path>           PEM TLS private key file (requires --tls-cert)
  --allowed-origins <urls>   Comma-separated allowed Origin URLs for CSRF protection
  --master-key-file <path>   Path to master key file for envelope encryption (P4g)
  --generate-master-key <path>  Generate a new master key to this path and exit (P4g)
  --scrub-level <level>       Secret scrubbing: off|standard|aggressive (default: standard)
  --ephemeral                Use in-memory store (no durability)
  --shutdown-timeout <dur>   Graceful drain deadline (default: 10s)
  --help                     Show this help

The daemon reads wakil.yaml for backend/model credentials. The workspace ID
is derived from the config's working directory (same as the TUI).

With --http-addr, the daemon also serves the web console on that TCP address.
Connect RPCs are available at /wakil.v1alpha1.<Service>/<Method> (HTTP/JSON).

With --tls-cert and --tls-key, the TCP listener uses TLS (HTTPS). Session
cookies get the Secure flag. --allowed-origins sets the CSRF Origin allowlist
(required for production/hosted mode).

With --master-key-file, backend API keys are envelope-encrypted at rest
(AES-256-GCM). Use --generate-master-key to create a new key file. Without a
master key, backend management RPCs return Unimplemented.

--scrub-level controls secret redaction in trace events at write time.
Standard (default) redacts known API key patterns; aggressive adds generic
high-entropy detection; off disables scrubbing.
`

func run() error {
	flags, err := parseFlags(os.Args[1:])
	if err != nil {
		return err
	}

	// Handle --generate-master-key: generate and exit.
	if flags.generateMasterKey != "" {
		key, err := crypto.GenerateMasterKey()
		if err != nil {
			return fmt.Errorf("wakild: generate master key: %w", err)
		}
		if err := crypto.WriteMasterKeyFile(flags.generateMasterKey, key); err != nil {
			return fmt.Errorf("wakild: write master key: %w", err)
		}
		fmt.Fprintf(os.Stderr, "wakild: master key written to %s (permissions 0600)\n", flags.generateMasterKey)
		return nil
	}

	// Validate TLS flag combinations.
	if flags.tlsKeyFile != "" && flags.tlsCertFile == "" {
		return fmt.Errorf("wakild: --tls-key requires --tls-cert")
	}
	if flags.tlsCertFile != "" && flags.tlsKeyFile == "" {
		return fmt.Errorf("wakild: --tls-cert requires --tls-key")
	}
	if flags.tlsCertFile != "" && flags.httpAddr == "" {
		return fmt.Errorf("wakild: --tls-cert requires --http-addr (TLS applies to the TCP listener)")
	}

	// Validate scrub level.
	scrubLevel := flags.scrubLevel
	if scrubLevel == "" {
		scrubLevel = "standard" // default
	}
	if _, err := scrub.ParseLevel(scrubLevel); err != nil {
		return err
	}

	// Load the same config the TUI reads (wakil.yaml).
	cfg, err := config.LoadConfig(nil)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	wsID := wiring.WorkspaceIDFromConfig(cfg)

	ds, err := newDaemonServer(cfg, flags.socketPath, flags.ephemeral, wsID, flags.httpAddr, flags.tlsCertFile, flags.tlsKeyFile, flags.allowedOrigins, flags.masterKeyFile, scrubLevel)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "wakild: listening on %s (ephemeral=%v, workspace=%s)\n",
		flags.socketPath, flags.ephemeral, wsID)
	if flags.httpAddr != "" {
		scheme := "http"
		if flags.tlsCertFile != "" {
			scheme = "https"
		}
		fmt.Fprintf(os.Stderr, "wakild: web console at %s://%s/\n", scheme, flags.httpAddr)
	}

	// Serve until signal.
	ctx := waitForSignal(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- ds.serve()
	}()

	select {
	case err := <-serveErr:
		// Serve exited (error or clean stop).
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "wakild: serve error: %v\n", err)
		}
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "wakild: shutting down...")
	}

	// Graceful shutdown with deadline.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), flags.shutdownTimeout)
	defer cancel()
	ds.shutdown(shutdownCtx)

	// Remove the socket file if it still exists.
	if err := os.Remove(flags.socketPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "wakild: remove socket: %v\n", err)
	}

	fmt.Fprintln(os.Stderr, "wakild: stopped")
	return nil
}
