package connect

import (
	"net/http"

	wakilv1alpha1connect "github.com/treeol/wakil/api/gen/wakil/v1alpha1/wakilv1alpha1connect"
	"github.com/treeol/wakil/internal/auth/apitoken"
	"github.com/treeol/wakil/internal/auth/jointoken"
	"github.com/treeol/wakil/internal/auth/tokenstore"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/sessionhost"
)

// Server bundles the Connect service handlers and provides an
// http.Handler that mounts all of them.
type Server struct {
	session    *SessionHandler
	event      *EventHandler
	system     *SystemHandler
	auth       *AuthHandler
	resolver   principalResolver
	authIssuer *jointoken.Issuer // nil if auth not configured
	apiIssuer  *apitoken.Issuer  // nil if api token management not configured
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

// NewServerWithAuth creates a Connect server with AuthService support.
// The authIssuer provides join token management and exchange; the
// tokenStore backs web session lookups. The resolver is the principal
// resolver used by all service handlers. The apiIssuer provides API token
// management (may be nil if API tokens are not configured).
func NewServerWithAuth(host *sessionhost.Host, ephemeral bool, resolver principalResolver, authIssuer *jointoken.Issuer, apiIssuer *apitoken.Issuer, tokenStore *tokenstore.Store) *Server {
	if resolver == nil {
		panic("connect: NewServerWithAuth requires a non-nil principal resolver")
	}
	s := &Server{
		session:    NewSessionHandler(host, host, resolver),
		event:      NewEventHandler(host, host, resolver),
		system:     NewSystemHandler(ephemeral),
		resolver:   resolver,
		authIssuer: authIssuer,
		apiIssuer:  apiIssuer,
		auth:       NewAuthHandler(authIssuer, apiIssuer, tokenStore, resolver),
	}
	return s
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

// NewServerFromInterfacesWithAuth creates a Connect server with auth support
// from explicit interface implementations. Useful for testing with mocks.
func NewServerFromInterfacesWithAuth(svc core.SessionService, reader core.EventReader, snap core.SessionReader, ephemeral bool, resolver principalResolver, authIssuer *jointoken.Issuer, apiIssuer *apitoken.Issuer, tokenStore *tokenstore.Store) *Server {
	if resolver == nil {
		panic("connect: NewServerFromInterfacesWithAuth requires a non-nil principal resolver")
	}
	return &Server{
		session:    NewSessionHandler(svc, snap, resolver),
		event:      NewEventHandler(reader, snap, resolver),
		system:     NewSystemHandler(ephemeral),
		resolver:   resolver,
		authIssuer: authIssuer,
		apiIssuer:  apiIssuer,
		auth:       NewAuthHandler(authIssuer, apiIssuer, tokenStore, resolver),
	}
}

// Handler returns an http.Handler that serves all Connect services,
// wrapped in the header injector middleware (so the token/cookie resolver
// can read HTTP headers from the context).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	path, handler := wakilv1alpha1connect.NewSessionServiceHandler(s.session)
	mux.Handle(path, handler)
	path2, handler2 := wakilv1alpha1connect.NewEventServiceHandler(s.event)
	mux.Handle(path2, handler2)
	path3, handler3 := wakilv1alpha1connect.NewSystemServiceHandler(s.system)
	mux.Handle(path3, handler3)
	if s.auth != nil {
		path4, handler4 := wakilv1alpha1connect.NewAuthServiceHandler(s.auth)
		mux.Handle(path4, handler4)
	}
	return headerInjector(mux)
}
