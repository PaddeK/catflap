//go:build !linux && !darwin && !windows

package setup

import (
	"errors"

	"github.com/PaddeK/catflap/internal/homelab"
)

func platformService(homelab.Paths) (homelabService, error) {
	return nil, errors.New("catflap has no homelab service for this OS; run `catflap homelab run` yourself")
}
