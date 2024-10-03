package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mitchellh/go-ps"
	log "github.com/sirupsen/logrus"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path"
	"podman-wsl-service/pkg/proxy"
	"podman-wsl-service/pkg/wslpath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

type podmanProxy struct {
	sharedRoot   string
	upstream     string
	downstream   string
	client       http.Client
	server       http.Server
	versionRegex *regexp.Regexp
}

type PodmanProxy interface {
	TestUpstreamSocket() error
	Serve() error
}

type contextKey struct {
	key string
}

var ConnContextKey = &contextKey{"http-conn"}

func saveConnInContext(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, ConnContextKey, c)
}

func getHttpConn(r *http.Request) *net.UnixConn {
	return r.Context().Value(ConnContextKey).(*net.UnixConn)
}

func NewPodmanProxy(sharedRoot string, upstreamSocket string, downstreamSocket string) PodmanProxy {
	proxy := &podmanProxy{
		sharedRoot:   sharedRoot,
		upstream:     upstreamSocket,
		downstream:   downstreamSocket,
		versionRegex: regexp.MustCompile(`^/v\d+.(?:\d\.?)+/`),
	}
	proxy.client = http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", proxy.upstream)
			},
		},
	}
	proxy.server = http.Server{
		Handler:     proxy,
		ConnContext: saveConnInContext,
	}

	return proxy
}

func (p *podmanProxy) TestUpstreamSocket() error {
	resp, err := p.client.Get("http://d/_ping")
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return err
	}
	if err = resp.Body.Close(); err != nil {
		return err
	}
	return nil
}

func (p *podmanProxy) Serve() error {
	socketDir := path.Dir(p.downstream)
	if err := os.MkdirAll(socketDir, 0755); err != nil {
		return err
	}
	unixListener, err := net.Listen("unix", p.downstream)
	if err != nil {
		return err
	}

	if err := os.Chmod(p.downstream, 0660); err != nil {
		return err
	}

	idleConnsClosed := make(chan struct{})
	go func() {
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, os.Interrupt)
		<-sigint

		// We received an interrupt signal, shut down.
		if err := p.server.Shutdown(context.Background()); err != nil {
			// Error from closing listeners, or context timeout:
			log.Printf("HTTP server Shutdown: %v", err)
		}
		close(idleConnsClosed)
	}()

	log.Infof("Listening on %s\n", p.downstream)
	if err := p.server.Serve(unixListener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	<-idleConnsClosed
	return nil
}

func (p *podmanProxy) translateHostPath(hostPath string) (string, error) {
	if strings.HasPrefix(hostPath, "/mnt/wsl/") {
		return hostPath, nil
	}
	winPath, err := wslpath.ToWindows(hostPath)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(winPath, "\\\\wsl.localhost\\") {
		// Local WSL path
		if !strings.HasPrefix(hostPath, "/") {
			return "", fmt.Errorf("PODMAN WSL SERVICE BUG: unexpected path format, expected absolute path: '%s'", hostPath)
		}
		return path.Join(p.sharedRoot, strings.TrimLeft(hostPath, "/")), nil
	}
	// Windows path
	return winPath, nil
}

func (p *podmanProxy) mangleLibpodVolumes(body map[string]interface{}) error {
	mounts, ok := body["mounts"].([]map[string]string)
	if !ok {
		log.Debugf("mounts field not found in request body, assuming no volumes to translate\n")
		return nil
	}
	for _, mount := range mounts {
		if mount["type"] != "bind" {
			continue
		}
		hostPath := mount["source"]
		newHostPath, err := p.translateHostPath(hostPath)
		if err != nil {
			return err
		}
		mount["source"] = newHostPath
	}
	return nil
}

func (p *podmanProxy) mangleDockerVolumes(body map[string]interface{}) error {
	var newBinds []interface{}
	hostConfig, ok := body["HostConfig"].(map[string]interface{})
	if !ok {
		log.Debugln("HostConfig field not found in request body, assuming no volumes to translate")
		return nil
	}
	binds, ok := hostConfig["Binds"].([]interface{})
	if !ok {
		return nil
	}
	for _, bind := range binds {
		if bind, ok := bind.(string); ok {
			parts := strings.Split(bind, ":")
			hostPath := parts[0]
			newHostPath, err := p.translateHostPath(hostPath)
			if err != nil {
				return err
			}
			parts[0] = newHostPath
			newBinds = append(newBinds, strings.Join(parts, ":"))
		} else {
			newBinds = append(newBinds, bind)
		}
	}
	hostConfig["Binds"] = newBinds
	return nil
}

func loggerWithProcessInfo(conn *net.UnixConn, oldLogger *log.Entry) (logger *log.Entry) {
	logger = oldLogger
	if conn == nil {
		return
	}

	connFile, err := conn.File()
	if err != nil || connFile == nil {
		logger.Errorf("Error getting peer file descriptor: %v", err)
		return
	}

	ucred, err := syscall.GetsockoptUcred(int(connFile.Fd()), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	if err != nil {
		logger.Errorf("Error getting peer credentials: %v\n", err)
		return
	}

	userName := strconv.Itoa(int(ucred.Uid))
	if userInfo, err := user.LookupId(userName); err == nil {
		userName = userInfo.Username
	}

	logger = logger.WithFields(log.Fields{
		"pid":  ucred.Pid,
		"user": userName,
	})

	if process, err := ps.FindProcess(int(ucred.Pid)); err == nil {
		logger = logger.WithField("program", process.Executable())
	}

	return
}

type flusherWriter struct {
	w io.Writer
}

func (fw *flusherWriter) Write(p []byte) (n int, err error) {
	if f, ok := fw.w.(http.Flusher); ok {
		defer f.Flush()
	}
	return fw.w.Write(p)
}

func (p *podmanProxy) forwardRequest(dsWriter http.ResponseWriter, r *http.Request, logger *log.Entry) {
	usReq := r.Clone(context.Background())

	// Convert server request to client request
	usReq.RequestURI = ""
	usReq.URL.Scheme = "http"
	usReq.URL.Host = usReq.Header.Get("Host")
	usReq.Proto = "HTTP/1.1"
	if usReq.URL.Host == "" {
		usReq.URL.Host = "d"
	}
	if strings.ToLower(usReq.Header.Get("Connection")) != "upgrade" {
		usReq.Header.Set("Connection", "close")
	}

	// Dial the upstream server socket manually so we can take over if the response is a WebSocket upgrade
	usConn, err := net.Dial("unix", p.upstream)
	if err != nil {
		logger.Errorf("Error connecting to upstream: %v\n", err)
		http.Error(dsWriter, err.Error(), http.StatusBadGateway)
		return
	}

	// Close the upstream connection when we're done
	defer func(usConn net.Conn) {
		err := usConn.Close()
		if err != nil {
			if !strings.Contains(err.Error(), "use of closed network connection") {
				logger.Errorf("Error closing downstream connection: %v\n", err)
			}
		}
	}(usConn)

	// Write the request to the upstream socket
	if err := usReq.Write(usConn); err != nil {
		logger.Errorf("Error performing request: %v\n", err)
		http.Error(dsWriter, err.Error(), http.StatusBadGateway)
		return
	}

	// Read the response from the upstream socket
	usReader := bufio.NewReader(usConn)
	usResp, err := http.ReadResponse(usReader, usReq)
	if err != nil {
		logger.Errorf("Error reading response: %v\n", err)
		http.Error(dsWriter, err.Error(), http.StatusInternalServerError)
		return
	}
	logger = logger.WithField("status", usResp.StatusCode)

	logger.Infof("Status: %d\n", usResp.StatusCode)

	// Change connection header to close if it's not an upgrade
	if strings.ToLower(usResp.Header.Get("Connection")) != "upgrade" {
		usResp.Header.Set("Connection", "close")
	}

	// Handle protocols
	if usResp.StatusCode != http.StatusSwitchingProtocols {
		for k, v := range usResp.Header {
			dsWriter.Header()[k] = v
		}
		dsWriter.WriteHeader(usResp.StatusCode)

		flushWriter := &flusherWriter{w: dsWriter}

		// Regular HTTP response, just copy the body
		if _, err := io.Copy(flushWriter, usResp.Body); err != nil {
			logger.Errorf("Error copying response body: %v\n", err)
		}
	} else {
		// Take over and forward WebSocket communication
		// FIXME: Hijack() doesn't work with Unix sockets
		// https://github.com/golang/go/issues/69741
		//dsConn, dsReadWriter, err := http.NewResponseController(dsWriter).Hijack()

		// BEGIN WORKAROUND
		dsWriter.(http.Flusher).Flush()
		dsFile, err := getHttpConn(r).File()
		if err != nil {
			logger.Errorf("Error getting downstream file descriptor: %v\n", err)
			http.Error(dsWriter, err.Error(), http.StatusInternalServerError)
			return
		}
		dsFD := int(dsFile.Fd())
		//newDsFd, err := syscall.Dup(dsFD)
		//if err != nil {
		//	logger.Errorf("Error duplicating downstream file descriptor: %v\n", err)
		//	http.Error(dsWriter, err.Error(), http.StatusInternalServerError)
		//	return
		//}

		dsConn, err := net.FileConn(os.NewFile(uintptr(dsFD), "downstream-socket"))
		if err != nil {
			logger.Panicf("Error creating downstream connection: %v\n", err)
		}
		//dsReadWriter := bufio.NewReadWriter(bufio.NewReader(dsConn), bufio.NewWriter(dsConn))

		//err = syscall.Close(dsFD)
		//goland:noinspection GoUnhandledErrorResult
		go dsWriter.(http.Hijacker).Hijack()
		// END WORKAROUND

		//if err != nil {
		//	logger.Errorf("Error hijacking connection: %v\n", err)
		//	return
		//}

		if err := proxy.ProxyFileConn(usConn.(*net.UnixConn), dsConn.(*net.UnixConn), logger); err != nil && !errors.Is(err, io.EOF) {
			logger.Errorf("Error proxying connection: %v\n", err)
		}

		if err := dsConn.Close(); err != nil {
			logger.Errorf("Error closing downstream connection: %v\n", err)
		}
		if err := usConn.Close(); err != nil {
			logger.Errorf("Error closing upstream connection: %v\n", err)
		}

		logger.Infoln("WebSocket connection closed")
	}
}

func (p *podmanProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := r.Method
	path := r.URL.Path
	if p.versionRegex.MatchString(path) {
		path = p.versionRegex.ReplaceAllString(path, "/")
	}

	contentType := r.Header.Get("Content-Type")

	logger := log.WithFields(log.Fields{
		"method":  method,
		"path":    path,
		"changed": false,
	})

	conn := getHttpConn(r)
	logger = loggerWithProcessInfo(conn, logger)

	if contentType != "" {
		logger = logger.WithField("content-type", contentType)
	}

	if method != "POST" || (path != "/containers/create" && path != "/libpod/containers/create") {
		p.forwardRequest(w, r, logger)
		return
	}

	if contentType != "application/json" && contentType != "" {
		logger.Warningln("Unsupported content type, passing request through")
		p.forwardRequest(w, r, logger)
		return
	}

	logger = logger.WithField("changed", true)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		logger.Errorf("Error reading request body: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err = r.Body.Close(); err != nil {
		logger.Errorf("Error closing request body: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	bodyObj := map[string]interface{}{}
	jsonDecoder := json.NewDecoder(bytes.NewReader(body))
	jsonDecoder.UseNumber()
	if err = jsonDecoder.Decode(&bodyObj); err != nil {
		logger.Errorf("Error decoding request body: %v\n", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if strings.HasPrefix(path, "/libpod") {
		err = p.mangleLibpodVolumes(bodyObj)
	} else {
		err = p.mangleDockerVolumes(bodyObj)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	newBody, err := json.Marshal(bodyObj)
	if err != nil {
		logger.Errorf("Error encoding modified request body: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	r.Body = io.NopCloser(bytes.NewReader(newBody))
	r.ContentLength = int64(len(newBody))
	r.Header.Set("Content-Length", strconv.FormatInt(r.ContentLength, 10))

	p.forwardRequest(w, r, logger)
}
