package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Alia5/VIIPER/internal/configpaths"
	"github.com/Alia5/VIIPER/internal/log"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/internal/server/api/auth"
	"github.com/Alia5/VIIPER/internal/server/api/handler"
	"github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/internal/tray"
	"github.com/Alia5/VIIPER/viipertypes"
)

const keyFileName = "viiper.key.txt"

type Server struct {
	USBServerConfig   usb.ServerConfig `embed:"" prefix:"usb."`
	APIServerConfig   api.ServerConfig `embed:"" prefix:"api."`
	ConnectionTimeout time.Duration    `help:"ConnectionTimeout operation timeout" default:"30s" env:"VIIPER_CONNECTION_TIMEOUT"`
	Transport         string           `help:"Virtual USB transport: usbip or native-ude" default:"usbip" env:"VIIPER_TRANSPORT"`
	KeyFile           string           `help:"Path to the API credential file." env:"VIIPER_KEY_FILE" type:"path"`
	serviceMode       bool
	ready             func()
}

// Run is called by Kong when the server command is executed.
func (s *Server) Run(logger *slog.Logger, rawLogger log.RawLogger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return s.StartServer(ctx, logger, rawLogger)
}

func (s *Server) StartServer(ctx context.Context, logger *slog.Logger, rawLogger log.RawLogger) error {
	transport := strings.ToLower(strings.TrimSpace(s.Transport))
	if transport != "usbip" && transport != "native-ude" {
		return fmt.Errorf("unsupported VIIPER transport %q (expected usbip or native-ude)", s.Transport)
	}
	if transport == "usbip" {
		if err := requireUSBIPRuntime(); err != nil {
			logger.Error("Refusing to start VIIPER with an incompatible USB/IP runtime", "error", err)
			return err
		}
	}
	applyTransportAPISecurityPolicy(transport, &s.APIServerConfig)

	ctx, cancel := context.WithCancel(ctx)
	stopTray := func() {}
	if !s.serviceMode {
		stopTray = tray.Run(ctx, cancel)
	}
	defer func() {
		cancel()
		stopTray()
	}()

	s.USBServerConfig.ConnectionTimeout = s.ConnectionTimeout
	s.APIServerConfig.ConnectionTimeout = s.ConnectionTimeout
	s.USBServerConfig.BusCleanupTimeout = s.APIServerConfig.DeviceHandlerConnectTimeout

	logger.Info("Starting VIIPER virtual USB server", "transport", transport,
		"usbipAddr", s.USBServerConfig.Addr)

	keyFilePath := strings.TrimSpace(s.KeyFile)
	if keyFilePath == "" {
		keyFileDir, err := configpaths.KeyFileDir()
		if err != nil {
			return fmt.Errorf("failed to resolve key file path: %w", err)
		}
		keyFilePath = filepath.Join(keyFileDir, keyFileName)
	} else if !filepath.IsAbs(keyFilePath) {
		return fmt.Errorf("API credential path must be absolute: %s", keyFilePath)
	}
	keyFileDir := filepath.Dir(keyFilePath)
	if pwd, err := os.ReadFile(keyFilePath); err == nil {
		s.APIServerConfig.Password = strings.TrimSpace(string(pwd))
		if s.APIServerConfig.Password == "" {
			return fmt.Errorf("API credential file is empty: %s", keyFilePath)
		}
	} else {
		if s.serviceMode {
			return fmt.Errorf("managed service API credential is missing or unreadable at %s: %w", keyFilePath, err)
		}
		newPwd, err := auth.GenerateKey()
		if err != nil {
			return fmt.Errorf("failed to generate new API password: %w", err)
		}
		if err := os.MkdirAll(keyFileDir, 0o700); err != nil {
			return fmt.Errorf("failed to create config dir for key file: %w", err)
		}
		if err := os.WriteFile(keyFilePath, []byte(newPwd), 0o600); err != nil {
			return fmt.Errorf("failed to write new API password to file: %w", err)
		}
		s.APIServerConfig.Password = newPwd
		logGeneratedAPICredential(logger, keyFilePath)
	}

	usbSrv := usb.New(s.USBServerConfig, logger, rawLogger)

	var usbErrCh <-chan error
	var nativeSession nativeUDETransport
	if transport == "usbip" {
		errors := make(chan error, 1)
		usbErrCh = errors
		go func() {
			errors <- usbSrv.ListenAndServe()
		}()
		select {
		case err := <-usbErrCh:
			return err
		case <-usbSrv.Ready():
		}
	} else {
		var err error
		nativeSession, err = startNativeUDETransport(ctx, usbSrv)
		if err != nil {
			return fmt.Errorf("start native UDE transport: %w", err)
		}
		defer nativeSession.Close()
		logger.Info("Starting VIIPER native UDE transport")
	}

	if s.APIServerConfig.Addr == "" {
		logger.Error("API server address must be set", "default", api.DefaultListenAddress)
		return fmt.Errorf("API server address must be set (default %s)", api.DefaultListenAddress)
	}

	apiSrv := api.New(usbSrv, s.APIServerConfig.Addr, s.APIServerConfig, logger)
	r := apiSrv.Router()
	r.Register("ping", handler.Ping(handler.PingOptions{
		Transport: transport,
		Status: func() (bool, *viipertypes.NativeUDEInfo) {
			if nativeSession != nil {
				return nativeSession.Status()
			}
			return true, nil
		},
	}))
	r.Register("bus/list", handler.BusList(usbSrv))
	r.Register("bus/create", handler.BusCreate(usbSrv))
	r.Register("bus/remove", handler.BusRemove(usbSrv))
	r.Register("bus/{id}/list", handler.BusDevicesList(usbSrv))
	r.Register("bus/{id}/add", handler.BusDeviceAdd(usbSrv, apiSrv))
	r.Register("bus/{id}/remove", handler.BusDeviceRemove(usbSrv))
	r.Register("bus/{id}/remove-native", handler.BusDeviceRemoveNative(usbSrv))
	r.RegisterStream("bus/{busId}/{deviceid}", api.DeviceStreamHandler(usbSrv))

	if s.APIServerConfig.AutoAttachLocalClient && transport == "usbip" {
		logger.Info("Auto-attach is enabled, checking prerequisites...")
		if !api.CheckAutoAttachPrerequisites(s.APIServerConfig.AutoAttachWindowsNative, logger) {
			logger.Warn("Auto-attach prerequisites not met")
			logger.Warn("Device auto-attachment will fail until requirements are satisfied")
			logger.Info("You can disable auto-attach with --api.auto-attach-local-client=false")
		} else {
			logger.Info("Auto-attach prerequisites satisfied")
		}
	}

	if err := apiSrv.Start(); err != nil {
		logger.Error("failed to start API server", "error", err)
		return err
	}
	if s.ready != nil {
		s.ready()
	}

	select {
	case <-ctx.Done():
		if apiSrv != nil {
			apiSrv.Close()
		}
		if transport == "usbip" {
			_ = usbSrv.Close()
			_ = <-usbErrCh // nolint
		}
		return nil
	case err := <-usbErrCh:
		if apiSrv != nil {
			apiSrv.Close()
		}
		return err
	case err := <-nativeDone(nativeSession):
		if apiSrv != nil {
			apiSrv.Close()
		}
		if err == nil && ctx.Err() == nil {
			return errors.New("native UDE transport stopped unexpectedly")
		}
		return err
	}
}

func applyTransportAPISecurityPolicy(transport string, config *api.ServerConfig) {
	// The native broker owns local kernel topology and live controller streams.
	// It must never inherit the historical unauthenticated-localhost exemption,
	// particularly when the broker is eventually hosted as LocalSystem.
	if transport == "native-ude" {
		config.RequireLocalHostAuth = true
	}
}

func logGeneratedAPICredential(logger *slog.Logger, path string) {
	logger.Info("Generated API server credential", "path", path)
	logger.Info("API clients must authenticate with the credential stored in that file")
}

func nativeDone(session nativeUDETransport) <-chan error {
	if session == nil {
		return nil
	}
	return session.Done()
}
