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
	session *SessionHandler
	event   *EventHandler
	system  *SystemHandler
}

// NewServer creates a Connect server backed by the given host.
// ephemeral reports whether the store is in-memory (advertised via
// GetServerInfo).
func NewServer(host *sessionhost.Host, ephemeral bool) *Server {
	return &Server{
		session: NewSessionHandler(host, host),
		event:   NewEventHandler(host, host),
		system:  NewSystemHandler(ephemeral),
	}
}

// NewServerFromInterfaces creates a Connect server from explicit interface
// implementations. Useful for testing with mocks.
func NewServerFromInterfaces(svc core.SessionService, reader core.EventReader, snap core.SessionReader, ephemeral bool) *Server {
	return &Server{
		session: NewSessionHandler(svc, snap),
		event:   NewEventHandler(reader, snap),
		system:  NewSystemHandler(ephemeral),
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

// HandlerWithStatic returns an http.Handler that serves the Connect services
// alongside the provided static file handler. The static handler is mounted at
// "/" and handles any path not claimed by a Connect service.
func (s *Server) HandlerWithStatic(staticHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	path, handler := wakilv1alpha1connect.NewSessionServiceHandler(s.session)
	mux.Handle(path, handler)
	path2, handler2 := wakilv1alpha1connect.NewEventServiceHandler(s.event)
	mux.Handle(path2, handler2)
	path3, handler3 := wakilv1alpha1connect.NewSystemServiceHandler(s.system)
	mux.Handle(path3, handler3)
	mux.Handle("/", staticHandler)
	return mux
}
