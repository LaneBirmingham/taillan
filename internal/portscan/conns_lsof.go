//go:build darwin || freebsd

package portscan

import (
	"context"

	psnet "github.com/shirou/gopsutil/v4/net"
)

// connections lists sockets. gopsutil doesn't implement the "without
// UIDs" variant on macOS and FreeBSD (it returns "not implemented"),
// so use the lsof-based one. lsof only sees this user's processes
// unless run as root, which covers games the player started.
func connections(ctx context.Context) ([]psnet.ConnectionStat, error) {
	return psnet.ConnectionsWithContext(ctx, "inet")
}
