package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Alia5/VIIPER/device/xboxone"
	"github.com/Alia5/VIIPER/internal/log"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/internal/server/api/handler"
	"github.com/Alia5/VIIPER/internal/server/usb"
	"github.com/Alia5/VIIPER/internal/tray"
)

const keyFileName = "viiper.key.txt"

type Server struct {
	USBServerConfig   usb.ServerConfig `embed:"" prefix:"usb."`
	APIServerConfig   api.ServerConfig `embed:"" prefix:"api."`
	ConnectionTimeout time.Duration    `help:"ConnectionTimeout operation timeout" default:"30s" env:"VIIPER_CONNECTION_TIMEOUT"`
	KeyFile           *string          `help:"Explicit absolute API password file; errors never fall back to the default key file"`
}

// Run is called by Kong when the server command is executed.
func (s *Server) Run(logger *slog.Logger, rawLogger log.RawLogger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return s.StartServer(ctx, logger, rawLogger)
}

func (s *Server) StartServer(ctx context.Context, logger *slog.Logger, rawLogger log.RawLogger) error {
	keyFilePath, err := resolveServerKeyFilePath(s.KeyFile)
	if err != nil {
		return err
	}
	if err := requireUSBIPRuntime(); err != nil {
		logger.Error("Refusing to start VIIPER with an incompatible USB/IP runtime", "error", err)
		return err
	}

	password, generated, err := loadServerAPIKey(keyFilePath, s.KeyFile != nil)
	if err != nil {
		return err
	}
	s.APIServerConfig.Password = password
	if generated {
		logger.Info("Generated API server password", "path", keyFilePath)
		if s.KeyFile == nil {
			// Preserve normal first-run output. Explicit deployments share the
			// key file with their client and must not leak it to lab logs.
			logger.Info("-------------------------------------")
			logger.Info("Your VIIPER API server password is:")
			logger.Info("-------------------------------------")
			logger.Info(password)
			logger.Info("-------------------------------------")
			logger.Info("You can change this password at any time by editing the file")
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	stopTray := tray.Run(ctx, cancel)
	defer func() {
		cancel()
		stopTray()
	}()

	s.USBServerConfig.ConnectionTimeout = s.ConnectionTimeout
	s.APIServerConfig.ConnectionTimeout = s.ConnectionTimeout
	s.USBServerConfig.BusCleanupTimeout = s.APIServerConfig.DeviceHandlerConnectTimeout

	logger.Info("Starting VIIPER USB-IP server", "addr", s.USBServerConfig.Addr)

	usbSrv := usb.New(s.USBServerConfig, logger, rawLogger)
	return runOwnedServers(ctx, usbSrv.ListenAndServe, usbSrv.Ready(), usbSrv.Close,
		func() (func(), error) {
			if s.APIServerConfig.Addr == "" {
				logger.Error("API server address must be set (default :3242).")
				return nil, fmt.Errorf("API server address must be set (default :3242).") // nolint
			}

			apiSrv := api.New(usbSrv, s.APIServerConfig.Addr, s.APIServerConfig, logger)
			api.RegisterStreamHandler("xboxone", xboxone.ProductionStreamHandler)
			r := apiSrv.Router()
			r.Register("ping", handler.Ping())
			r.Register("bus/list", handler.BusList(usbSrv))
			r.Register("bus/create", handler.BusCreate(usbSrv))
			r.Register("bus/remove", handler.BusRemove(usbSrv))
			r.Register("bus/{id}/list", handler.BusDevicesList(usbSrv))
			r.Register("bus/{id}/add", handler.BusDeviceAdd(usbSrv, apiSrv))
			r.Register("bus/{id}/add-authorized-xboxone",
				handler.BusDeviceAddAuthorizedXboxOne(usbSrv, apiSrv))
			r.Register("bus/{busId}/{devId}/activate-authorized-xboxone",
				handler.BusDeviceActivateAuthorizedXboxOne(usbSrv, apiSrv))
			r.Register("bus/{busId}/{devId}/remove-authorized-xboxone",
				handler.BusDeviceRemoveAuthorizedXboxOne(usbSrv))
			r.Register("bus/{id}/remove", handler.BusDeviceRemove(usbSrv))
			r.Register("bus/{busId}/{devId}/microphone-interface",
				handler.BusDeviceMicrophoneInterfaceStatus(usbSrv))
			r.Register("bus/{busId}/{devId}/ns2pro-status-v1",
				handler.BusDeviceNS2ProRuntimeStatusV1(usbSrv))
			r.RegisterStream("bus/{busId}/{deviceid}", api.DeviceStreamHandler(usbSrv))
			r.RegisterStream("bus/{busId}/{deviceid}/stream-authorized-xboxone", api.DeviceStreamHandler(usbSrv))

			if s.APIServerConfig.AutoAttachLocalClient {
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
				return apiSrv.Close, err
			}
			return apiSrv.Close, nil
		})
}

// runOwnedServers contains the actual attempt-owned listener lifecycle, with
// cold dependencies that permit failure-path tests without opening sockets.
func runOwnedServers(ctx context.Context, serveUSB func() error, usbReady <-chan struct{},
	closeUSB func() error, startAPI func() (func(), error)) error {
	usbErrCh := make(chan error, 1)
	go func() { usbErrCh <- serveUSB() }()
	var closeAPI func()
	usbJoined := false
	defer func() {
		if closeAPI != nil {
			closeAPI()
		}
		// Close only this attempt, including partial API startup. Preserve the
		// original startup/serve error rather than replacing it with cleanup.
		_ = closeUSB()
		if !usbJoined {
			<-usbErrCh
		}
	}()
	select {
	case err := <-usbErrCh:
		usbJoined = true
		return err
	case <-usbReady:
	case <-ctx.Done():
		return nil
	}
	// Ready and cancellation may become observable together. A cancelled
	// attempt must not create a new API listener while being retired.
	if ctx.Err() != nil {
		return nil
	}
	var err error
	closeAPI, err = startAPI()
	if err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return nil
	case err := <-usbErrCh:
		usbJoined = true
		return err
	}
}
