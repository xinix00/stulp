//go:build darwin

package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// platformBrowse is a host-development escape hatch for macOS. Stulp's
// normal discovery path is the pure-Go multicast implementation. Some VPN
// network extensions reject application-owned multicast sockets while
// Apple's mDNSResponder continues to work; dns-sd talks to that daemon and
// lets the same binary remain usable on those Macs. Hopy/no-OS builds never
// compile this adapter.
func platformBrowse(ctx context.Context, window time.Duration, services []string) ([]Node, error) {
	if window <= 0 {
		window = 3 * time.Second
	}
	platformCtx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	browseWindow := window / 2
	if browseWindow < 500*time.Millisecond {
		browseWindow = 500 * time.Millisecond
	}
	if browseWindow > 1500*time.Millisecond {
		browseWindow = 1500 * time.Millisecond
	}

	type result struct {
		service string
		output  []byte
		err     error
	}
	results := make(chan result, len(services))
	for _, service := range services {
		go func(service string) {
			output, err := dnsServiceZone(platformCtx, service, browseWindow)
			results <- result{service: service, output: output, err: err}
		}(service)
	}

	records := newCollector()
	var runErrors []error
	for range services {
		result := <-results
		if result.err != nil {
			runErrors = append(runErrors, fmt.Errorf("%s: %w", result.service, result.err))
			continue
		}
		parseDNSServiceZone(records, result.service, string(result.output))
	}
	if len(runErrors) == len(services) {
		return nil, errors.Join(runErrors...)
	}
	// Commissioning needs an address immediately, while operational records
	// can be resolved lazily after their fabric/node ID has been matched. This
	// avoids launching a resolver process for every Matter fabric visible on a
	// busy smart-home LAN.
	for instance, service := range records.kinds {
		if service != ServiceCommissionable {
			continue
		}
		record, ok := records.services[instance]
		if !ok || record.host == "" {
			continue
		}
		addresses, err := platformResolve(platformCtx, record.host)
		if err == nil {
			records.addresses[record.host] = addresses
		}
	}

	return records.nodes(), nil
}

func dnsServiceZone(ctx context.Context, service string, window time.Duration) ([]byte, error) {
	browseCtx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	shortService := strings.TrimSuffix(service, ".local.")
	command := exec.CommandContext(browseCtx, "/usr/bin/dns-sd", "-Z", shortService, "local.")
	output, err := command.Output()
	if len(output) > 0 && errors.Is(browseCtx.Err(), context.DeadlineExceeded) {
		return output, nil
	}
	if err != nil {
		return nil, err
	}
	return output, nil
}

func parseDNSServiceZone(records *collector, service, output string) {
	shortService := strings.TrimSuffix(service, ".local.")
	ownerSuffix := "." + shortService
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || !strings.HasSuffix(fields[0], ownerSuffix) {
			continue
		}
		instance := fields[0] + ".local."
		// The collector keys instances by the service they were advertised
		// under; nodes() turns that back into a kind.
		records.kinds[instance] = service
		switch fields[1] {
		case "SRV":
			if len(fields) < 6 {
				continue
			}
			port, err := strconv.ParseUint(fields[4], 10, 16)
			if err != nil {
				continue
			}
			host := strings.TrimSuffix(fields[5], ".") + "."
			records.services[instance] = serviceRecord{host: host, port: uint16(port)}
		case "TXT":
			records.texts[instance] = append(records.texts[instance], quotedZoneValues(line)...)
		}
	}
}

// quotedZoneValues splits a dns-sd zone line into its quoted TXT values, byte
// for byte.
//
// A TXT value is arbitrary octets, not text: a Thread border router publishes
// its extended PAN ID as eight raw bytes. Decoding through runes would replace
// every byte that is not valid UTF-8, so the escapes are handled directly and
// everything else is copied unchanged.
func quotedZoneValues(line string) []string {
	var values []string
	for index := 0; index < len(line); index++ {
		if line[index] != '"' {
			continue
		}
		var value []byte
		for index++; index < len(line) && line[index] != '"'; index++ {
			if line[index] == '\\' && index+1 < len(line) {
				index++
			}
			value = append(value, line[index])
		}
		if index >= len(line) {
			// An unterminated value is a truncated line, not a value.
			break
		}
		values = append(values, string(value))
	}
	return values
}

func platformResolve(ctx context.Context, host string) ([]string, error) {
	command := exec.CommandContext(ctx, "/usr/bin/dns-sd", "-G", "v4v6", strings.TrimSuffix(host, ".")+".")
	output, err := command.Output()
	if len(output) == 0 && err != nil {
		return nil, err
	}
	addresses := parseDNSServiceAddresses(string(output))
	if len(addresses) == 0 {
		return nil, fmt.Errorf("mDNSResponder returned no address for %s", host)
	}
	sort.SliceStable(addresses, func(left, right int) bool {
		return matterAddressRank(addresses[left]) < matterAddressRank(addresses[right])
	})
	return addresses, nil
}

func parseDNSServiceAddresses(output string) []string {
	var addresses []string
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 || fields[1] != "Add" {
			continue
		}
		rawAddress, _, _ := strings.Cut(fields[5], "%")
		ip := net.ParseIP(rawAddress)
		if ip == nil {
			continue
		}
		address := ip.String()
		if ip.IsLinkLocalUnicast() {
			index, indexErr := strconv.Atoi(fields[3])
			if indexErr == nil {
				if networkInterface, interfaceErr := net.InterfaceByIndex(index); interfaceErr == nil {
					address += "%" + networkInterface.Name
				}
			}
		}
		addresses = appendUnique(addresses, address)
	}
	return addresses
}
