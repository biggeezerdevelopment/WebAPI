package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"webapi/tlsconfig"
)

// Syslog transports.
const (
	SyslogNetworkTLS = "tls"
	SyslogNetworkTCP = "tcp"
	SyslogNetworkUDP = "udp"
)

// syslogFacilities maps RFC 5424 facility names to their numeric codes.
var syslogFacilities = map[string]int{
	"kern": 0, "user": 1, "mail": 2, "daemon": 3, "auth": 4, "syslog": 5,
	"lpr": 6, "news": 7, "uucp": 8, "cron": 9, "authpriv": 10, "ftp": 11,
	"ntp": 12, "audit": 13, "alert": 14, "clock": 15,
	"local0": 16, "local1": 17, "local2": 18, "local3": 19,
	"local4": 20, "local5": 21, "local6": 22, "local7": 23,
}

// syslogSeverities maps RFC 5424 severity names to their numeric codes.
var syslogSeverities = map[string]int{
	"emerg": 0, "alert": 1, "crit": 2, "err": 3,
	"warning": 4, "notice": 5, "info": 6, "debug": 7,
}

// SyslogPriority returns the RFC 5424 PRI value for a facility and severity.
func SyslogPriority(facility, severity string) (int, bool) {
	f, okFacility := syslogFacilities[facility]
	s, okSeverity := syslogSeverities[severity]
	if !okFacility || !okSeverity {
		return 0, false
	}
	return f*8 + s, true
}

// validateSyslog leaves configured values out of its errors, because a
// misplaced value could be a credential.
func (o *LogOutput) validateSyslog() error {
	switch o.Network {
	case SyslogNetworkTLS, SyslogNetworkTCP, SyslogNetworkUDP:
	default:
		return errors.New("unknown network: must be tls, tcp, or udp")
	}
	if !isHostPort(o.Address) {
		return errors.New("address must be a valid host:port with a port between 1 and 65535")
	}
	if _, ok := syslogFacilities[o.Facility]; !ok {
		return errors.New("unknown facility: must be an RFC 5424 name such as local0")
	}
	if _, ok := syslogSeverities[o.Severity]; !ok {
		return errors.New("unknown severity: must be emerg, alert, crit, err, warning, notice, info, or debug")
	}
	if err := o.validateTimeout(); err != nil {
		return err
	}
	if o.Network != SyslogNetworkTLS {
		if o.tlsSet || o.TLS != (tlsconfig.Options{}) {
			return errors.New("tls settings require network tls")
		}
		return nil
	}
	if _, err := tlsconfig.Build(o.TLS); err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	return nil
}

// isHostPort reports whether s is a hostname, IPv4 address, or bracketed IPv6
// address, with a port in 1-65535.
func isHostPort(s string) bool {
	host, portText, err := net.SplitHostPort(s)
	if err != nil || host == "" {
		return false
	}
	if port, err := strconv.ParseUint(portText, 10, 16); err != nil || port == 0 {
		return false
	}
	addr, addrErr := netip.ParseAddr(host)
	// SplitHostPort removes brackets from any host, so check for them here.
	if strings.HasPrefix(s, "[") {
		return addrErr == nil && addr.Is6()
	}
	return addrErr == nil || isHostname(host)
}

// isHostname reports whether host is a DNS name of at most 253 bytes, with an
// optional root dot. A numeric final label is refused because no top-level
// domain is numeric, so such a host is a malformed IPv4 address.
func isHostname(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 {
		return false
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if !isHostnameLabel(label) {
			return false
		}
	}
	return strings.Trim(labels[len(labels)-1], "0123456789") != ""
}

// isHostnameLabel allows underscores, which service names such as _syslog use,
// but requires a label to end in a letter or digit and not start with a hyphen.
func isHostnameLabel(label string) bool {
	const alphanumeric = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	if label == "" || len(label) > 63 || label[0] == '-' {
		return false
	}
	return strings.Trim(label, alphanumeric+"-_") == "" &&
		strings.IndexByte(alphanumeric, label[len(label)-1]) >= 0
}
