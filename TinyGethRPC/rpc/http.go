package rpc

import (
    "io"
    "fmt"
	"log"
    "bytes"
    "context"
	"errors"
	"mime"
	"net/http"
    "encoding/json"
    "sync"
    "sync/atomic"
    "time"
    mapset "github.com/deckarep/golang-set"
)

const (
	maxRequestContentLength = 1024 * 1024 * 5
	contentType             = "application/json"
)
const MetadataApi = "rpc"
var acceptedContentTypes = []string{contentType, "application/json-rpc", "application/jsonrequest"}

// Server is an RPC server.
type Server struct {
	services serviceRegistry
	idgen    func() ID
	run      int32
	codecs   mapset.Set
}

type peerInfoContextKey struct{}


type httpConn struct {
	client    *http.Client
	url       string
	closeOnce sync.Once
	closeCh   chan interface{}
	mu        sync.Mutex // protects headers
	headers   http.Header
}

// httpConn implements ServerCodec, but it is treated specially by Client
// and some methods don't work. The panic() stubs here exist to ensure
// this special treatment is correct.

func (hc *httpConn) writeJSON(context.Context, interface{}) error {
	panic("writeJSON called on httpConn")
}

func (hc *httpConn) peerInfo() PeerInfo {
	panic("peerInfo called on httpConn")
}

func (hc *httpConn) remoteAddr() string {
	return hc.url
}

func (hc *httpConn) readBatch() ([]*jsonrpcMessage, bool, error) {
	<-hc.closeCh
	return nil, false, io.EOF
}

func (hc *httpConn) close() {
	hc.closeOnce.Do(func() { close(hc.closeCh) })
}

func (hc *httpConn) closed() <-chan interface{} {
	return hc.closeCh
}

func (hc *httpConn) doRequest(ctx context.Context, msg interface{}) (io.ReadCloser, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", hc.url, io.NopCloser(bytes.NewReader(body)))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }

	// set headers
	hc.mu.Lock()
	req.Header = hc.headers.Clone()
	hc.mu.Unlock()

	// do request
	resp, err := hc.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var buf bytes.Buffer
		var body []byte
		if _, err := buf.ReadFrom(resp.Body); err == nil {
			body = buf.Bytes()
		}

		return nil, HTTPError{
			Status:     resp.Status,
			StatusCode: resp.StatusCode,
			Body:       body,
		}
	}
	return resp.Body, nil
}

// httpServerConn turns a HTTP connection into a Conn.
type httpServerConn struct {
	io.Reader
	io.Writer
	r *http.Request
}



// Close does nothing and always returns nil.
func (t *httpServerConn) Close() error { return nil }

// RemoteAddr returns the peer address of the underlying connection.
func (t *httpServerConn) RemoteAddr() string {
	return t.r.RemoteAddr
}

// SetWriteDeadline does nothing and always returns nil.
func (t *httpServerConn) SetWriteDeadline(time.Time) error { return nil }


// RPCService gives meta information about the server.
// e.g. gives information about the loaded modules.
type RPCService struct {
	server *Server
}

// NewServer creates a new server instance with no registered handlers.
func NewServer() *Server {
	server := &Server{idgen: randomIDGenerator(), codecs: mapset.NewSet(), run: 1}
	// Register the default service providing meta information about the RPC service such
	// as the services and methods it offers.
	rpcService := &RPCService{server}
	server.RegisterName(MetadataApi, rpcService)
	return server
}

// RegisterName creates a service for the given receiver type under the given name. When no
// methods on the given receiver match the criteria to be either a RPC method or a
// subscription an error is returned. Otherwise a new service is created and added to the
// service collection this server provides to clients.
func (s *Server) RegisterName(name string, receiver interface{}) error {
	return s.services.registerName(name, receiver)
}


// ServeHTTP serves JSON-RPC requests over HTTP.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Permit dumb empty requests for remote health-checks (AWS)
	if r.Method == http.MethodGet && r.ContentLength == 0 && r.URL.RawQuery == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if code, err := validateRequest(r); err != nil {
		http.Error(w, err.Error(), code)
		return
	}

	// Create request-scoped context.
	connInfo := PeerInfo{Transport: "http", RemoteAddr: r.RemoteAddr}
	connInfo.HTTP.Version = r.Proto
	connInfo.HTTP.Host = r.Host
	connInfo.HTTP.Origin = r.Header.Get("Origin")
	connInfo.HTTP.UserAgent = r.Header.Get("User-Agent")
	ctx := r.Context()
	ctx = context.WithValue(ctx, peerInfoContextKey{}, connInfo)
	// All checks passed, create a codec that reads directly from the request body
	// until EOF, writes the response to w, and orders the server to process a
	// single request.
	w.Header().Set("content-type", contentType)
	codec := newHTTPServerConn(r, w)
	defer codec.close()
	s.serveSingleRequest(ctx, codec)
}

// serveSingleRequest reads and processes a single RPC request from the given codec. This
// is used to serve HTTP connections. Subscriptions and reverse calls are not allowed in
// this mode.
func (s *Server) serveSingleRequest(ctx context.Context, codec ServerCodec) {
	// Don't serve if server is stopped.
	if atomic.LoadInt32(&s.run) == 0 {
		return
	}
	h := newHandler(ctx, codec, s.idgen, &s.services)
	defer h.close(io.EOF, nil)

	reqs, batch, err := codec.readBatch()
	if err != nil {
		if err != io.EOF {
			codec.writeJSON(ctx, errorMessage(&invalidMessageError{"parse error"}))
		}
		return
	}
	if batch {
		h.handleBatch(reqs)
	} else {
		h.handleMsg(reqs[0])
	}
}

// validateRequest returns a non-zero response code and error message if the
// request is invalid.
func validateRequest(r *http.Request) (int, error) {
	if r.Method == http.MethodPut || r.Method == http.MethodDelete {
		return http.StatusMethodNotAllowed, errors.New("method not allowed")
	}
	if r.ContentLength > maxRequestContentLength {
		err := fmt.Errorf("content length too large (%d>%d)", r.ContentLength, maxRequestContentLength)
		return http.StatusRequestEntityTooLarge, err
	}
	// Allow OPTIONS (regardless of content-type)
	if r.Method == http.MethodOptions {
		return 0, nil
	}
	// Check content-type
	if mt, _, err := mime.ParseMediaType(r.Header.Get("content-type")); err == nil {
		for _, accepted := range acceptedContentTypes {
			if accepted == mt {
				return 0, nil
			}
		}
	}
	// Invalid content-type
	err := fmt.Errorf("invalid content type, only %s is supported", contentType)
	return http.StatusUnsupportedMediaType, err
}


func newHTTPServerConn(r *http.Request, w http.ResponseWriter) ServerCodec {
	body := io.LimitReader(r.Body, maxRequestContentLength)
	conn := &httpServerConn{Reader: body, Writer: w, r: r}
    fmt.Println(conn)
    return NewCodec(conn)
}


// Stop stops reading new requests, waits for stopPendingRequestTimeout to allow pending
// requests to finish, then closes all codecs which will cancel pending requests and
// subscriptions.
func (s *Server) Stop() {
	if atomic.CompareAndSwapInt32(&s.run, 1, 0) {
		log.Println("Http RPC server shutting down")
		s.codecs.Each(func(c interface{}) bool {
			c.(ServerCodec).close()
			return true
		})
	}
}
