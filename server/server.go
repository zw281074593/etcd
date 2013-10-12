package server

import (
	"fmt"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	etcdErr "github.com/coreos/etcd/error"
	"github.com/coreos/etcd/log"
	"github.com/coreos/etcd/server/v1"
	"github.com/coreos/etcd/store"
	"github.com/coreos/go-raft"
	"github.com/gorilla/mux"
)

// This is the default implementation of the Server interface.
type Server struct {
	http.Server
	raftServer  *raft.Server
    registry    *Registry
    store       *store.Store
	name        string
	url         string
	tlsConf     *TLSConfig
	tlsInfo     *TLSInfo
	corsOrigins map[string]bool
}

// Creates a new Server.
func New(name string, urlStr string, listenHost string, tlsConf *TLSConfig, tlsInfo *TLSInfo, raftServer *raft.Server, registry *Registry, store *store.Store) *Server {
	s := &Server{
		Server: http.Server{
			Handler:   mux.NewRouter(),
			TLSConfig: &tlsConf.Server,
			Addr:      listenHost,
		},
		store:      store,
		registry:   registry,
		url:        urlStr,
		tlsConf:    tlsConf,
		tlsInfo:    tlsInfo,
		raftServer: raftServer,
	}

	// Install the routes for each version of the API.
	s.installV1()

	return s
}

// The current Raft committed index.
func (s *Server) CommitIndex() uint64 {
	return s.raftServer.CommitIndex()
}

// The current Raft term.
func (s *Server) Term() uint64 {
	return s.raftServer.Term()
}

// The server URL.
func (s *Server) URL() string {
	return s.url
}

// Returns a reference to the Store.
func (s *Server) Store() *store.Store {
	return s.store
}

func (s *Server) installV1() {
	s.handleFuncV1("/v1/keys/{key:.*}", v1.GetKeyHandler).Methods("GET")
	s.handleFuncV1("/v1/keys/{key:.*}", v1.SetKeyHandler).Methods("POST", "PUT")
	s.handleFuncV1("/v1/keys/{key:.*}", v1.DeleteKeyHandler).Methods("DELETE")

	s.handleFuncV1("/v1/watch/{key:.*}", v1.WatchKeyHandler).Methods("GET", "POST")
}

// Adds a v1 server handler to the router.
func (s *Server) handleFuncV1(path string, f func(http.ResponseWriter, *http.Request, v1.Server) error) *mux.Route {
	return s.handleFunc(path, func(w http.ResponseWriter, req *http.Request, s *Server) error {
		return f(w, req, s)
	})
}

// Adds a server handler to the router.
func (s *Server) handleFunc(path string, f func(http.ResponseWriter, *http.Request, *Server) error) *mux.Route {
	r := s.Handler.(*mux.Router)

	// Wrap the standard HandleFunc interface to pass in the server reference.
	return r.HandleFunc(path, func(w http.ResponseWriter, req *http.Request) {
		// Log request.
		log.Debugf("[recv] %s %s [%s]", req.Method, s.url, req.URL.Path, req.RemoteAddr)

		// Write CORS header.
		if s.OriginAllowed("*") {
			w.Header().Add("Access-Control-Allow-Origin", "*")
		} else if origin := req.Header.Get("Origin"); s.OriginAllowed(origin) {
			w.Header().Add("Access-Control-Allow-Origin", origin)
		}

		// Execute handler function and return error if necessary.
		if err := f(w, req, s); err != nil {
			if etcdErr, ok := err.(*etcdErr.Error); ok {
				log.Debug("Return error: ", (*etcdErr).Error())
				etcdErr.Write(w)
			} else {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		}
	})
}

// Start to listen and response etcd client command
func (s *Server) ListenAndServe() {
	log.Infof("etcd server [name %s, listen on %s, advertised url %s]", s.name, s.Server.Addr, s.url)

	if s.tlsConf.Scheme == "http" {
		log.Fatal(s.Server.ListenAndServe())
	} else {
		log.Fatal(s.Server.ListenAndServeTLS(s.tlsInfo.CertFile, s.tlsInfo.KeyFile))
	}
}

func (s *Server) Dispatch(c raft.Command, w http.ResponseWriter, req *http.Request) error {
	if s.raftServer.State() == raft.Leader {
		event, err := s.raftServer.Do(c)
		if err != nil {
			return err
		}

		if event == nil {
			return etcdErr.NewError(300, "Empty result from raft", store.UndefIndex, store.UndefTerm)
		}

		response := event.(*store.Event).Response()
		b, _ := json.Marshal(response)
		w.WriteHeader(http.StatusOK)
		w.Write(b)

		return nil

	} else {
		leader := s.raftServer.Leader()

		// No leader available.
		if leader == "" {
			return etcdErr.NewError(300, "", store.UndefIndex, store.UndefTerm)
		}

		url, _ := s.registry.PeerURL(leader)
		redirect(url, w, req)

		return nil
	}
}

// Sets a comma-delimited list of origins that are allowed.
func (s *Server) AllowOrigins(origins string) error {
	// Construct a lookup of all origins.
	m := make(map[string]bool)
	for _, v := range strings.Split(origins, ",") {
		if v != "*" {
			if _, err := url.Parse(v); err != nil {
				return fmt.Errorf("Invalid CORS origin: %s", err)
			}
		}
		m[v] = true
	}
	s.corsOrigins = m

	return nil
}

// Determines whether the server will allow a given CORS origin.
func (s *Server) OriginAllowed(origin string) bool {
	return s.corsOrigins["*"] || s.corsOrigins[origin]
}
