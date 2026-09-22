package tunnel

import "github.com/theboysclash/WindowOsUrlPOrt/internal/config"

func configFor(mode, token string) config.Tunnel {
	return config.Tunnel{Enabled: true, Mode: mode, Token: token}
}
