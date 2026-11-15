//go:build !darwin && !freebsd

package portscan

import (
	"context"

	psnet "github.com/shirou/gopsutil/v4/net"
)

// connections lists sockets, skipping the per-socket UID lookups we
// don't need.
func connections(ctx context.Context) ([]psnet.ConnectionStat, error) {
	return psnet.ConnectionsWithoutUidsWithContext(ctx, "inet")
}
