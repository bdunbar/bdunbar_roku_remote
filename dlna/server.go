package dlna

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

// Server is a DLNA Digital Media Server: an SSDP presence plus an HTTP
// service that describes itself, answers Browse, and serves bytes.
type Server struct {
	Library      *Library
	FriendlyName string

	// Logf receives one line per renderer request. Nil discards them.
	Logf func(format string, args ...any)

	udn  string
	ln   net.Listener
	srv  *http.Server
	ssdp *ssdpResponder
	ip   net.IP
}

// NewServer builds a media server for a directory. The name is what shows
// up in the TV's list of media servers.
func NewServer(root, friendlyName string) (*Server, error) {
	lib, err := NewLibrary(root)
	if err != nil {
		return nil, err
	}
	if friendlyName == "" {
		host, _ := os.Hostname()
		if host == "" {
			host = "laptop"
		}
		friendlyName = host + " media"
	}
	return &Server{
		Library:      lib,
		FriendlyName: friendlyName,
		udn:          makeUDN(friendlyName, lib.Root),
	}, nil
}

// Addr reports where the server is listening, once started.
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Start binds the HTTP service and begins advertising over SSDP. It returns
// as soon as the server is reachable; Run blocks.
func (s *Server) Start(port int) error {
	ip, err := lanIP()
	if err != nil {
		return err
	}
	s.ip = ip

	// Bind to the LAN address specifically. Advertising a URL on an address
	// the TV cannot reach is the single most common way a DLNA server
	// appears in the list but plays nothing.
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", ip, port))
	if err != nil {
		return err
	}
	s.ln = ln

	mux := http.NewServeMux()
	mux.HandleFunc("/device.xml", s.handleDeviceXML)
	mux.HandleFunc("/cds.xml", func(w http.ResponseWriter, r *http.Request) {
		serveXML(w, contentDirectorySCPD)
	})
	mux.HandleFunc("/cms.xml", func(w http.ResponseWriter, r *http.Request) {
		serveXML(w, connectionManagerSCPD)
	})
	mux.HandleFunc("/control/cds", s.handleControl)
	mux.HandleFunc("/control/cms", s.handleControl)
	mux.HandleFunc("/media/", s.handleMedia)
	s.srv = &http.Server{
		Handler: s.logging(mux),
		// No write timeout: serving a two-hour film is a legitimate long
		// response, and a timeout here would cut playback off mid-scene.
		ReadHeaderTimeout: 10 * time.Second,
	}

	location := fmt.Sprintf("http://%s/device.xml", ln.Addr().String())
	if s.ssdp, err = newSSDPResponder(s.udn, location); err != nil {
		ln.Close()
		return err
	}
	go s.srv.Serve(ln)
	return nil
}

// Run advertises the server and blocks until the context is cancelled,
// then withdraws the advertisement and shuts down cleanly.
func (s *Server) Run(ctx context.Context) error {
	go s.ssdp.run(ctx)
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.srv.Shutdown(shutdownCtx)
}

func (s *Server) handleDeviceXML(w http.ResponseWriter, r *http.Request) {
	serveXML(w, deviceDescription(s.udn, s.FriendlyName))
}

// handleMedia serves an item's bytes.
//
// http.ServeContent does the real work, including range requests, which is
// what lets the TV seek. The DLNA headers on top tell the renderer that
// seeking is available before it tries.
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	// The URL carries the object ID with the original extension appended,
	// because some renderers trust the extension over the Content-Type.
	id := strings.TrimPrefix(path.Base(r.URL.Path), "/")
	if i := strings.Index(id, "."); i >= 0 {
		id = id[:i]
	}
	obj, ok := s.Library.Get(id)
	if !ok || obj.IsContainer() {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(obj.Path)
	if err != nil {
		http.Error(w, "cannot open media", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "cannot stat media", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", obj.MIME)
	w.Header().Set("transferMode.dlna.org", transferMode(obj))
	w.Header().Set("contentFeatures.dlna.org", contentFeatures(obj))
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, obj.Path, info.ModTime(), f)
}

// transferMode tells the renderer what kind of delivery this is: streaming
// media it will play continuously, or a block it will fetch and display.
func transferMode(o *Object) string {
	if o.Class == ClassImage {
		return "Interactive"
	}
	return "Streaming"
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Logf != nil {
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			s.Logf("%s %s %s", host, r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

// baseURLFor builds the URL prefix a renderer should use for resources.
// Echoing back the Host it dialled is more reliable than assuming our own
// address, on a machine with more than one.
func (s *Server) baseURLFor(r *http.Request) string {
	host := r.Host
	if host == "" {
		host = s.ln.Addr().String()
	}
	return "http://" + host
}

func serveXML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	io.WriteString(w, body)
}

// lanIP picks the address other devices on the network can reach us on.
// Dialling a UDP socket sends nothing but makes the kernel perform the
// routing lookup, which is the decision we want.
func lanIP() (net.IP, error) {
	conn, err := net.Dial("udp", "239.255.255.250:1900")
	if err != nil {
		return nil, fmt.Errorf("finding the LAN address: %w", err)
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP, nil
}
