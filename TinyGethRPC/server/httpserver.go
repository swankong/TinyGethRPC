package server

import (
	"fmt"
    "log"
	"net"
    "time"
    "context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"TinyGethRPC/rpc"
    "github.com/rs/cors"
)


// HTTPTimeouts represents the configuration params for the HTTP RPC server.
type HTTPTimeouts struct {
	// ReadTimeout is the maximum duration for reading the entire
	// request, including the body.
	//
	// Because ReadTimeout does not let Handlers make per-request
	// decisions on each request body's acceptable deadline or
	// upload rate, most users will prefer to use
	// ReadHeaderTimeout. It is valid to use them both.
	ReadTimeout time.Duration

	// WriteTimeout is the maximum duration before timing out
	// writes of the response. It is reset whenever a new
	// request's header is read. Like ReadTimeout, it does not
	// let Handlers make decisions on a per-request basis.
	WriteTimeout time.Duration

	// IdleTimeout is the maximum amount of time to wait for the
	// next request when keep-alives are enabled. If IdleTimeout
	// is zero, the value of ReadTimeout is used. If both are
	// zero, ReadHeaderTimeout is used.
	IdleTimeout time.Duration
}

var DefaultHTTPTimeouts = HTTPTimeouts{
	ReadTimeout:  30 * time.Second,
	WriteTimeout: 30 * time.Second,
	IdleTimeout:  120 * time.Second,
}


type rpcHandler struct {
	http.Handler
	server *rpc.Server
}

// httpConfig is the JSON-RPC/HTTP configuration.
type httpConfig struct {
	Modules            []string
	CorsAllowedOrigins []string
	Vhosts             []string
	prefix             string // path prefix on which to mount http handler
}


type httpServer struct {
	log      *log.Logger
	timeouts HTTPTimeouts
	mux      http.ServeMux // registered handlers go here

	mu       sync.Mutex
	server   *http.Server
	listener net.Listener // non-nil when server is running

	// HTTP RPC handler things.
	httpConfig  httpConfig
	httpHandler atomic.Value // *rpcHandler

	// These are set by setListenAddr.
	endpoint string
	host     string
	port     int

	handlerNames map[string]string
}

func NewHttpServer(log *log.Logger, timeouts HTTPTimeouts) *httpServer {
	h := &httpServer{log: log, timeouts: timeouts, handlerNames: make(map[string]string)}
	h.httpHandler.Store((*rpcHandler)(nil))
	return h
}


/////
func (h *httpServer) StartHttpRPC(rpcApis []rpc.API, httpPort int) error {

    initHttp := func(s *httpServer, apis []rpc.API, port int) error {
        if err := s.setListenAddr(Config.HttpHost, port); err != nil {
            return err
        }
        if err := h.EnableRPC(rpcApis, httpConfig{
            CorsAllowedOrigins: Config.HttpCors,
            Vhosts:             Config.HttpVirtualHosts,
            Modules:            Config.HttpModules,
            prefix:             Config.HttpPathPrefix,
        }); err != nil {
            return err
        }
        return nil
    }
    // Set up HTTP.
    // Configure HTTP.
    if err := initHttp(h, rpcApis, httpPort); err != nil {
        return err
    }
    // Start the servers
    if err := h.Start(); err != nil {
        return err
    }   
    return nil
}

func (h *httpServer) StopHttpRPC() {
    h.Stop()
}


// setListenAddr configures the listening address of the server.
// The address can only be set while the server isn't running.
func (h *httpServer) setListenAddr(host string, port int) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.listener != nil && (host != h.host || port != h.port) {
		return fmt.Errorf("HTTP server already running on %s", h.endpoint)
	}

	h.host, h.port = host, port
	h.endpoint = fmt.Sprintf("%s:%d", host, port)
	return nil
}

// listenAddr returns the listening address of the server.
func (h *httpServer) listenAddr() string {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.listener != nil {
		return h.listener.Addr().String()
	}
	return h.endpoint
}

// start starts the HTTP server if it is enabled and not already running.
func (h *httpServer) Start() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.endpoint == "" || h.listener != nil {
		return nil // already running or not configured
	}

	// Initialize the server.
	h.server = &http.Server{Handler: h}  // h.ServeHttp is registered here
	if h.timeouts != (HTTPTimeouts{}) {
		CheckTimeouts(&h.timeouts)
		h.server.ReadTimeout = h.timeouts.ReadTimeout
		h.server.WriteTimeout = h.timeouts.WriteTimeout
		h.server.IdleTimeout = h.timeouts.IdleTimeout
	}

	// Start the server.
	listener, err := net.Listen("tcp", h.endpoint)
	if err != nil {
		// If the server fails to start, we need to clear out the RPC 
		// configuration so they can be configured another time.
		h.disableRPC()
		return err
	}
	h.listener = listener
	go h.server.Serve(listener)

	// if server doesn't support RPC, return after logging
	if !h.rpcAllowed() {
		return nil
	}
	// Log http endpoint.
	h.log.Println("HTTP server started",
		"endpoint", listener.Addr(), 
		"prefix", h.httpConfig.prefix,
		"cors", strings.Join(h.httpConfig.CorsAllowedOrigins, ","),
		"vhosts", strings.Join(h.httpConfig.Vhosts, ","),
	)

	// Log all handlers mounted on server.
	var paths []string
	for path := range h.handlerNames {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	logged := make(map[string]bool, len(paths))
	for _, path := range paths {
		name := h.handlerNames[path]
		if !logged[name] {
			log.Println(name+" enabled", "url", "http://"+listener.Addr().String()+path)
			logged[name] = true
		}
	}
	return nil
}

func (h *httpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {  // register in net/http.Server declare
	// if http-rpc is enabled, try to serve request
	rpc := h.httpHandler.Load().(*rpcHandler)
    if rpc != nil {
		// First try to route in the mux.
		// Requests to a path below root are handled by the mux,
		// which has all the handlers registered via Sever.RegisterHandler.
		// These are made available when RPC is enabled.
		muxHandler, pattern := h.mux.Handler(r)
		if pattern != "" {
			muxHandler.ServeHTTP(w, r)
			return
		}

		if checkPath(r, h.httpConfig.prefix) {
			rpc.ServeHTTP(w, r)
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
}

// checkPath checks whether a given request URL matches a given path prefix.
func checkPath(r *http.Request, path string) bool {
	// if no prefix has been specified, request URL must be on root
	if path == "" {
		return r.URL.Path == "/"
	}
	// otherwise, check to make sure prefix matches
	return len(r.URL.Path) >= len(path) && r.URL.Path[:len(path)] == path
}

// validatePrefix checks if 'path' is a valid configuration value for the RPC prefix option.
func validatePrefix(what, path string) error {
	if path == "" {
		return nil
	}
	if path[0] != '/' {
		return fmt.Errorf(`%s RPC path prefix %q does not contain leading "/"`, what, path)
	}
	if strings.ContainsAny(path, "?#") {
		// This is just to avoid confusion. While these would match correctly (i.e. they'd
		// match if URL-escaped into path), it's not easy to understand for users when
		// setting that on the command line.
		return fmt.Errorf("%s RPC path prefix %q contains URL meta-characters", what, path)
	}
	return nil
}

// stop shuts down the HTTP server.
func (h *httpServer) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.doStop()
}

func (h *httpServer) doStop() {
	if h.listener == nil {
		return // not running
	}

	// Shut down the server.
	httpHandler := h.httpHandler.Load().(*rpcHandler)
	if httpHandler != nil {
		h.httpHandler.Store((*rpcHandler)(nil))
		httpHandler.server.Stop()
	}
	h.server.Shutdown(context.Background())
	h.listener.Close()
	h.log.Println("HTTP server stopped", "endpoint", h.listener.Addr())

	// Clear out everything to allow re-configuring it later.
	h.host, h.port, h.endpoint = "", 0, ""
	h.server, h.listener = nil, nil
}

// enableRPC turns on JSON-RPC over HTTP on the server.
func (h *httpServer) EnableRPC(apis []rpc.API, config httpConfig) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.rpcAllowed() {
		return fmt.Errorf("JSON-RPC over HTTP is already enabled")
	}

	// Create RPC server and handler.
	srv := rpc.NewServer()
	if err := RegisterApis(apis, config.Modules, srv, false); err != nil {
		return err
	}
	h.httpConfig = config
	h.httpHandler.Store(&rpcHandler{
		Handler: NewHTTPHandlerStack(srv, config.CorsAllowedOrigins, config.Vhosts),
		server:  srv,
	})
	return nil
}

// disableRPC stops the HTTP RPC handler. This is internal, the caller must hold h.mu.
func (h *httpServer) disableRPC() bool {
	handler := h.httpHandler.Load().(*rpcHandler)
	if handler != nil {
		h.httpHandler.Store((*rpcHandler)(nil))
		handler.server.Stop()
	}
	return handler != nil
}

// rpcAllowed returns true when JSON-RPC over HTTP is enabled.
func (h *httpServer) rpcAllowed() bool {
	return h.httpHandler.Load().(*rpcHandler) != nil
}

// RegisterApis checks the given modules' availability, generates an allowlist based on the allowed modules,
// and then registers all of the APIs exposed by the services.
func RegisterApis(apis []rpc.API, modules []string, srv *rpc.Server, exposeAll bool) error {
	// Generate the allow list based on the allowed modules
	allowList := make(map[string]bool)
	for _, module := range modules {
		allowList[module] = true
	}
	// Register all the APIs exposed by the services
	for _, api := range apis {
		if exposeAll || allowList[api.Namespace] || (len(allowList) == 0 && api.Public) {
			if err := srv.RegisterName(api.Namespace, api.Service); err != nil {
				return err
			}
		}
	}
	return nil
}


// CheckTimeouts ensures that timeout values are meaningful
func CheckTimeouts(timeouts *HTTPTimeouts) {
	if timeouts.ReadTimeout < time.Second {
		log.Println("Sanitizing invalid HTTP read timeout", "provided", timeouts.ReadTimeout, "updated", DefaultHTTPTimeouts.ReadTimeout)
		timeouts.ReadTimeout = DefaultHTTPTimeouts.ReadTimeout
	}
	if timeouts.WriteTimeout < time.Second {
		log.Println("Sanitizing invalid HTTP write timeout", "provided", timeouts.WriteTimeout, "updated", DefaultHTTPTimeouts.WriteTimeout)
		timeouts.WriteTimeout = DefaultHTTPTimeouts.WriteTimeout
	}
	if timeouts.IdleTimeout < time.Second {
		log.Println("Sanitizing invalid HTTP idle timeout", "provided", timeouts.IdleTimeout, "updated", DefaultHTTPTimeouts.IdleTimeout)
		timeouts.IdleTimeout = DefaultHTTPTimeouts.IdleTimeout
	}
}

// NewHTTPHandlerStack returns wrapped http-related handlers
func NewHTTPHandlerStack(srv http.Handler, cors []string, vhosts []string) http.Handler {
	// Wrap the CORS-handler within a host-handler
	handler := newCorsHandler(srv, cors)
    vhandler := newVHostHandler(vhosts, handler)
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {vhandler.next.ServeHTTP(w, r)})
    //return newGzipHandler(handler)
}

func newCorsHandler(srv http.Handler, allowedOrigins []string) http.Handler {
	// disable CORS support if user has not specified a custom CORS configuration
	if len(allowedOrigins) == 0 {
		return srv
	}
	c := cors.New(cors.Options{
		AllowedOrigins: allowedOrigins,
		AllowedMethods: []string{http.MethodPost, http.MethodGet},
		AllowedHeaders: []string{"*"},
		MaxAge:         600,
	})
	return c.Handler(srv)
}

// virtualHostHandler is a handler which validates the Host-header of incoming requests.
// Using virtual hosts can help prevent DNS rebinding attacks, where a 'random' domain name points to
// the service ip address (but without CORS headers). By verifying the targeted virtual host, we can
// ensure that it's a destination that the node operator has defined.
type virtualHostHandler struct {
	vhosts map[string]struct{}
	next   http.Handler
}

func newVHostHandler(vhosts []string, next http.Handler) *virtualHostHandler {
	vhostMap := make(map[string]struct{})
    for _, allowedHost := range vhosts {
		vhostMap[strings.ToLower(allowedHost)] = struct{}{}
	}
	return &virtualHostHandler{vhostMap, next}
}


