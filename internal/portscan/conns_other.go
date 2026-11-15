//go:build !linux

package portscan

import (
	"context"

	psnet "github.com/shirou/gopsutil/v4/net"
)

// connections lists sockets. gopsutil implements the cheaper "without
// UIDs" listing only on Linux; elsewhere it returns "not implemented",
// so use the full one (the OS socket tables on Windows, lsof on macOS,
// which sees only this user's processes unless run as root).
func connections(ctx context.Context) ([]psnet.ConnectionStat, error) {
	return psnet.ConnectionsWithContext(ctx, "inet")
}
