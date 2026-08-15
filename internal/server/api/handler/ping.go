package handler

import (
	"encoding/json"
	"log/slog"

	"github.com/Alia5/VIIPER/internal/codegen/common"
	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/viipertypes"
)

// PingOptions adds live backend proof to the legacy identity response. The
// variadic form intentionally preserves source compatibility for embedded API
// users that still call Ping() without transport metadata.
type PingOptions struct {
	Transport string
	Status    func() (ready bool, native *viipertypes.NativeUDEInfo)
}

// Ping returns a handler for the "ping" endpoint.
// It provides a minimal identity + version response.
func Ping(options ...PingOptions) api.HandlerFunc {
	var option PingOptions
	if len(options) != 0 {
		option = options[0]
	}
	return func(_ *api.Request, res *api.Response, logger *slog.Logger) error {
		ver, err := common.GetVersion()
		if err != nil {
			ver = common.Version
			if ver == "" {
				ver = "dev"
			}
			logger.Error("ping: invalid version format", "error", err, "version", ver)
		}

		payload := viipertypes.PingResponse{
			Server: "VIIPER", Version: ver, Transport: option.Transport,
		}
		if option.Status != nil {
			ready, native := option.Status()
			payload.Ready = &ready
			payload.NativeUDE = native
		}
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		res.JSON = string(b)
		return nil
	}
}
