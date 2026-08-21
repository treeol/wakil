package connect

import (
	"net/http"

	wakilv1alpha1connect "github.com/treeol/wakil/api/gen/wakil/v1alpha1/wakilv1alpha1connect"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/sessionhost"
)

// Server bundles the three Connect service handlers and provides an
// http.Handler that mounts all of them.
type Server struct {
	session  *SessionHandler
	event    *EventHandler
	system   *SystemHandler
	resolver principalResolver
}

// NewServer creates a Connect server backed by the given host.
// ephemeral reports whether the store is in-memory (advertised via
// GetServerInfo).
// resolver is the principal resolver used by every handler to resolve the
// caller's identity from the request context. It must not be nil — use
// NewEmbeddedResolver for tests that want the P2 embedded behavior.
func NewServer(host *sessionhost.Host, ephemeral bool, resolver principalResolver) *Server {
	if resolver == nil {
		panic("connect: NewServer requires a non-nil principal resolver")
	}
	return &Server{
		session:  NewSessionHandler(host, host, resolver),
		event:    NewEventHandler(host, host, resolver),
		system:   NewSystemHandler(ephemeral),
		resolver: resolver,
	}
}

// NewServerFromInterfaces creates a Connect server from explicit interface
// implementations. Useful for testing with mocks.
func NewServerFromInterfaces(svc core.SessionService, reader core.EventReader, snap core.SessionReader, ephemeral bool, resolver principalResolver) *Server {
	if resolver == nil {
		panic("connect: NewServerFromInterfaces requires a non-nil principal resolver")
	}
	return &Server{
		session:  NewSessionHandler(svc, snap, resolver),
		event:    NewEventHandler(reader, snap, resolver),
		system:   NewSystemHandler(ephemeral),
		resolver: resolver,
	}
}

// Handler returns an http.Handler that serves all three Connect services.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	path, handler := wakilv1alpha1connect.NewSessionServiceHandler(s.session)
	mux.Handle(path, handler)
	path2, handler2 := wakilv1alpha1connect.NewEventServiceHandler(s.event)
	mux.Handle(path2, handler2)
	path3, handler3 := wakilv1alpha1connect.NewSystemServiceHandler(s.system)
	mux.Handle(path3, handler3)
	return mux
}
