// Package remote implements the TUI's remote-client surface for the wakil daemon
// mode (card #148 P2e). When the user runs `wakil --daemon`, the TUI does not
// embed the agent loop; instead it dials the wakil daemon's Unix socket and
// drives the session over Connect-RPC.
//
// The package provides:
//   - Dial: a Unix-socket HTTP client usable with Connect-go service clients.
//   - RemoteEventPump: consumes the StreamEvents server-stream, converts proto
//     events to domain events, deduplicates by seq, and reconnects on drop.
//   - RemoteFacade: implements sessionclient.Facade by calling Connect RPCs.
//   - RemoteConversationManager: implements sessionclient.ConversationManager.
//
// The remote path mirrors the embedded path's contract: the TUI holds a Facade
// and a ConversationManager and drives them the same way. The difference is that
// every call travels over the Unix socket instead of an in-process method call.
package remote

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	wakilv1alpha1connect "github.com/treeol/wakil/api/gen/wakil/v1alpha1/wakilv1alpha1connect"
)

// Dial opens a Unix-socket-backed HTTP client connected to the wakil daemon
// at socketPath. The returned Clients bundle holds the three Connect service
// clients (Session, Event, System) all sharing one HTTP client.
//
// The baseURL is "http://unix" — a dummy host the Connect client requires.
// The Unix socket dialer replaces the TCP dialer, so the host is never
// resolved. The dummy host MUST be non-empty (Connect rejects empty baseURLs).
func Dial(socketPath string) (*Clients, error) {
	if socketPath == "" {
		return nil, fmt.Errorf("remote: socket path is empty")
	}
	// Verify the socket exists and is connectable before building the client.
	// This gives a clear error at startup rather than a confusing HTTP error
	// on the first RPC. For daemon-mode clients that predate SessionStateService
	// (older builds), this check passes but GetSessionState will fail at
	// call time — the RemoteFacade degrades to cached-zero state in that case.
	if _, err := os.Stat(socketPath); err != nil {
		return nil, fmt.Errorf("remote: socket %s not found — is `wakil daemon` running?: %w", socketPath, err)
	}
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("remote: cannot connect to socket %s: %w", socketPath, err)
	}
	conn.Close()

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			// DialContext is called for every HTTP request. The address args
			// are ignored — the Unix socket is the only transport.
			d := net.Dialer{}
			return d.DialContext(ctx, "unix", socketPath)
		},
		// The daemon is local; disable keepalive pooling to avoid stale
		// connections across a daemon restart. Each request dials fresh.
		DisableKeepAlives: true,
	}
	httpClient := &http.Client{
		Transport: transport,
		// No overall timeout: StreamEvents is a long-lived server stream.
		// Per-RPC timeouts are managed by the caller's context.
	}
	const baseURL = "http://unix"

	return &Clients{
		Session:      wakilv1alpha1connect.NewSessionServiceClient(httpClient, baseURL),
		Event:        wakilv1alpha1connect.NewEventServiceClient(httpClient, baseURL),
		System:       wakilv1alpha1connect.NewSystemServiceClient(httpClient, baseURL),
		SessionState: wakilv1alpha1connect.NewSessionStateServiceClient(httpClient, baseURL),
		http:         httpClient,
	}, nil
}

// Clients holds the Connect service clients for the daemon. SessionState may
// be nil when constructed by tests that only need Session/Event/System (and is
// safe to nil-check before use).
type Clients struct {
	Session      wakilv1alpha1connect.SessionServiceClient
	Event        wakilv1alpha1connect.EventServiceClient
	System       wakilv1alpha1connect.SystemServiceClient
	SessionState wakilv1alpha1connect.SessionStateServiceClient
	http         *http.Client
}

// Close releases the HTTP client's resources (idle connections).
func (c *Clients) Close() {
	if c.http != nil {
		c.http.CloseIdleConnections()
	}
}

// CheckHealth pings the daemon's Health RPC. Used at startup to verify the
// daemon is alive and responding.
func CheckHealth(ctx context.Context, c *Clients) error {
	resp, err := c.System.Health(ctx, connect.NewRequest(&v1alpha1.HealthRequest{}))
	if err != nil {
		return fmt.Errorf("remote: health check failed: %w", err)
	}
	if resp.Msg.Status != "ready" && resp.Msg.Status != "draining" {
		return fmt.Errorf("remote: daemon not ready (status=%q)", resp.Msg.Status)
	}
	return nil
}

// DefaultSocketPath returns the default Unix socket path, mirroring the
// daemon's defaultSocketPath().
func DefaultSocketPath() string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return xdg + "/wakil.sock"
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home + "/.local/share/wakil/wakil.sock"
	}
	return "wakil.sock"
}

// rpcTimeout is the default context timeout for unary RPCs (non-streaming).
const rpcTimeout = 30 * time.Second
