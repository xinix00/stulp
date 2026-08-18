//go:build !darwin

package discovery

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"
)

func platformBrowse(context.Context, time.Duration, []string) ([]Node, error) {
	return nil, errors.New("no platform mDNS fallback on this system")
}

func platformResolve(ctx context.Context, host string) ([]string, error) {
	resolved, err := net.DefaultResolver.LookupIPAddr(ctx, strings.TrimSuffix(host, "."))
	if err != nil {
		return nil, err
	}
	addresses := make([]string, 0, len(resolved))
	for _, candidate := range resolved {
		address := candidate.IP.String()
		if candidate.IP.IsLinkLocalUnicast() && candidate.Zone != "" {
			address += "%" + candidate.Zone
		}
		addresses = appendUnique(addresses, address)
	}
	return addresses, nil
}
