package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Alia5/VIIPER/internal/server/api/auth"
	apierror "github.com/Alia5/VIIPER/internal/server/api/error"
	"github.com/Alia5/VIIPER/internal/server/usb"
	pusb "github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/viipertypes"
)

// Server implements a small TCP API for managing virtual bus topology.
type Server struct {
	usbs          *usb.Server
	addr          string
	ln            net.Listener
	logger        *slog.Logger
	router        *Router
	config        *ServerConfig
	deviceStreams deviceStreamCoordinator
}

// microphonePCMResetter is implemented by audio-capable virtual controllers.
// Its reset is coordinated with stream ownership instead of individual device
// handlers so a same-device replacement can retain already-buffered capture.
type microphonePCMResetter interface {
	ResetMicrophonePCM()
}

// deviceStreamReconnectGrace covers the natural client lifecycle in which the
// old stream is closed immediately before its same-device replacement opens.
// Keeping this separate from the longer removal timeout preserves live capture
// audio without retaining stale transport state for the full device lifetime.
const deviceStreamReconnectGrace = 250 * time.Millisecond

// New creates a new ApiServer bound to a server.Server instance.
func New(s *usb.Server, addr string, config ServerConfig, logger *slog.Logger) *Server {
	cfg := config
	a := &Server{
		usbs:   s,
		addr:   addr,
		logger: logger,
		config: &cfg,
	}
	a.router = NewRouter()
	return a
}

// Router returns the router used by the API server so callers can register handlers.
func (s *Server) Router() *Router { return s.router }

// USB returns the underlying USB server.
func (s *Server) USB() *usb.Server { return s.usbs }

// Config returns the server configuration.
func (s *Server) Config() *ServerConfig { return s.config }

// ScheduleDeviceCleanup arms the initial no-stream cleanup through the same
// generation owner used for reconnects. A stream that claims the device before
// the timeout atomically cancels this cleanup.
func (s *Server) ScheduleDeviceCleanup(busID uint32, devID string,
	deviceContext context.Context) {
	key := deviceStreamKey{busID: busID, devID: devID}
	s.deviceStreams.scheduleCleanup(key,
		s.config.DeviceHandlerConnectTimeout, deviceContext, func() {
			if err := s.usbs.RemoveDeviceByID(busID, devID); err != nil {
				s.logger.Error("timeout: failed to remove device",
					"busID", busID, "deviceID", devID, "error", err)
			} else {
				s.logger.Info("timeout: removed device (no connection)",
					"busID", busID, "deviceID", devID)
			}
		})
}

// Addr returns the actual address the server is listening on.
// If Start hasn't been called yet, it returns the configured address.
func (s *Server) Addr() string {
	if s.ln != nil {
		return s.ln.Addr().String()
	}
	return s.addr
}

// Start listens on the configured address and serves incoming API commands.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.ln = ln

	s.addr = ln.Addr().String()
	s.config.Addr = s.addr
	s.logger.Info("API listening", "addr", s.addr)
	go s.serve()
	return nil
}

// Close stops the API server.
func (s *Server) Close() {
	if s.ln != nil {
		_ = s.ln.Close()
	}
}

func (s *Server) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || strings.Contains(strings.ToLower(err.Error()), "use of closed network connection") {
				s.logger.Info("API server stopped")
				return
			}
			s.logger.Info("API accept error", "error", err)
			return
		}
		if tcpConn, ok := c.(*net.TCPConn); ok {
			if err := tcpConn.SetNoDelay(true); err != nil {
				s.logger.Warn("failed to set TCP_NODELAY", "error", err)
			}
		}
		go s.handleConn(c)
	}
}

func (s *Server) writeError(w io.Writer, err error) {
	apiErr := apierror.WrapError(err)
	problemJSON, _ := json.Marshal(apiErr)
	_, err = fmt.Fprintf(w, "%s\n", string(problemJSON))
	if err != nil {
		s.logger.Error("failed to write error response", "error", err)
	}
}

func (s *Server) writeOK(w io.Writer, rest string) {
	if rest == "" {
		_, err := fmt.Fprintln(w)
		if err != nil {
			s.logger.Error("failed to write OK response", "error", err)
		}
	} else {
		_, err := fmt.Fprintf(w, "%s\n", rest)
		if err != nil {
			s.logger.Error("failed to write OK response", "error", err)
		}
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close() //nolint:errcheck

	connCtx, connCancel := context.WithCancel(context.Background())
	defer connCancel()

	connLogger := s.logger.With("remote", conn.RemoteAddr().String())
	r := bufio.NewReader(conn)
	w := conn

	isAuth, err := auth.IsAuthHandshake(r)
	if err != nil {
		connLogger.Error("api handshake check", "error", err)
		// continue as unauthenticated
	}

	if !isAuth && s.requiresAuth(conn.RemoteAddr()) {
		connLogger.Error("authentication required")
		s.writeError(w, apierror.ErrUnauthorized("authentication required"))
		return
	}

	if isAuth {
		connLogger.Debug("Detected auth attempt")
		key, err := auth.DeriveKey(s.config.Password)
		if err != nil {
			connLogger.Error("derive key failed", "error", err)
			return
		}

		clientNonce, serverNonce, err := auth.HandleAuthHandshake(r, w, key, false)
		if err != nil {
			if apiErr, ok := errors.AsType[viipertypes.APIError](err); ok {
				connLogger.Error("auth handshake failed", "error", apiErr)
				s.writeError(w, apiErr)
				return
			}
			connLogger.Error("auth handshake failed", "error", err)
			return
		}

		sessionKey := auth.DeriveSessionKey(key, serverNonce, clientNonce)
		secConn, err := auth.WrapConn(conn, sessionKey)
		if err != nil {
			connLogger.Error("wrap secure conn failed", "error", err)
			return
		}
		conn = secConn
		r = bufio.NewReader(conn)
		w = conn

		connLogger.Debug("authenticated connection established")
	} else {
		connLogger.Debug("continuing unauthenticated connection")
	}

	// Read until null terminator
	reqData, err := r.ReadString('\x00')
	if err != nil {
		if err == io.EOF {
			connLogger.Error("api incomplete request (no null terminator)")
		} else {
			connLogger.Error("read api data", "error", err)
		}
		return
	}
	// Remove null terminator
	reqData = strings.TrimSuffix(reqData, "\x00")

	if reqData == "" {
		connLogger.Error("api empty command")
		s.writeError(w, apierror.ErrBadRequest("empty request"))
		return
	}

	// Split on first whitespace character using regex \s
	wsRegex := regexp.MustCompile(`\s`)
	loc := wsRegex.FindStringIndex(reqData)

	var path, payload string
	if loc != nil {
		path = reqData[:loc[0]]
		payload = reqData[loc[1]:]
	} else {
		path = reqData
		payload = ""
	}

	if path == "" {
		connLogger.Error("api empty path")
		s.writeError(w, apierror.ErrBadRequest("empty path"))
		return
	}

	path = strings.ToLower(path)
	connLogger.Info("api cmd", "path", path)

	if h, params := s.router.Match(path); h != nil {
		req := &Request{Ctx: connCtx, Params: params, Payload: payload}
		res := &Response{}
		if err := h(req, res, connLogger); err != nil {
			connLogger.Error("api handler error", "path", path, "error", err)
			s.writeError(w, err)
			return
		}
		connLogger.Debug("api handler success", "path", path)
		s.writeOK(w, res.JSON)
		return
	} else if sh, params := s.router.MatchStream(path); sh != nil {
		connLogger.Info("api stream begin", "path", path)
		// ReadString can legally buffer bytes sent immediately after the stream
		// path. Keep that reader in front of the connection for the device
		// handler; otherwise the first input/microphone frame of a reconnect can
		// disappear in the handshake reader and stall framing indefinitely.
		streamConn := &bufferedReadConn{Conn: conn, reader: r}
		busIDStr, ok := params["busId"]
		if !ok {
			s.writeError(w, apierror.ErrBadRequest("missing busId parameter"))
			return
		}
		devIDStr, ok := params["deviceid"]
		if !ok {
			s.writeError(w, apierror.ErrBadRequest("missing deviceid parameter"))
			return
		}

		busID, err := strconv.ParseUint(busIDStr, 10, 32)
		if err != nil {
			s.writeError(w, apierror.ErrBadRequest(fmt.Sprintf("invalid busId: %v", err)))
			return
		}
		bus := s.usbs.GetBus(uint32(busID))
		if bus == nil {
			s.writeError(w, apierror.ErrNotFound(fmt.Sprintf("bus %d not found", busID)))
			return
		}
		var dev pusb.Device
		var devCtx context.Context
		metas := bus.GetAllDeviceMetas()
		for _, meta := range metas {
			if fmt.Sprintf("%d", meta.Meta.DevID) == devIDStr {
				dev = meta.Dev
				devCtx = bus.GetDeviceContext(dev)
				break
			}
		}
		if dev == nil || devCtx == nil {
			s.writeError(w, apierror.ErrNotFound(fmt.Sprintf("device %s not found on bus %d", devIDStr, busID)))
			return
		}

		streamKey := deviceStreamKey{busID: uint32(busID), devID: devIDStr}
		lease := s.deviceStreams.claim(streamKey, streamConn)
		handlerStarted := false
		defer func() {
			if !handlerStarted {
				lease.abandon()
				return
			}
			lease.finish(deviceStreamReconnectGrace,
				s.config.DeviceHandlerConnectTimeout, devCtx, func() {
					if resetter, ok := dev.(microphonePCMResetter); ok {
						resetter.ResetMicrophonePCM()
					}
				}, func() {
					if err := bus.RemoveDeviceByID(devIDStr); err != nil {
						connLogger.Error("disconnect timeout: failed to remove device",
							"busID", busID, "deviceID", devIDStr, "error", err)
					} else {
						connLogger.Info("disconnect timeout: removed device (no reconnection)",
							"busID", busID, "deviceID", devIDStr)
					}
				})
		}()

		if !lease.waitForTurn(devCtx) {
			return
		}
		select {
		case <-devCtx.Done():
			return
		default:
		}

		// Stream handler takes ownership of connection
		handlerStarted = true
		if err := sh(streamConn, &dev, connLogger); err != nil {
			connLogger.Error("api stream handler error", "path", path, "error", err)
		}
		connLogger.Info("api stream end", "path", path)

		return
	}
	connLogger.Error("api unknown path", "path", path)
	s.writeError(w, apierror.ErrNotFound(fmt.Sprintf("unknown path: %s", path)))
}

type bufferedReadConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedReadConn) Read(buffer []byte) (int, error) {
	return c.reader.Read(buffer)
}

func (s *Server) isLocalHostClient(addr net.Addr) bool {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return false
	}
	switch host {
	case "localhost", "127.0.0.1", "[::1]", "::1":
		return true
	}

	return false
}

func (s *Server) requiresAuth(addr net.Addr) bool {
	if s.isLocalHostClient(addr) {
		return s.config.RequireLocalHostAuth
	}
	return true
}
