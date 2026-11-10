package gateway

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

const wireGuardDNS = "10.77.0.1"

type DNSUpstream struct {
	Protocol string `json:"protocol"`
	Address  string `json:"address"`
	IP       string `json:"ip"`
	TLSName  string `json:"tlsName"`
}
type DNSSettings struct {
	Enabled         bool        `json:"enabled"`
	Listen          string      `json:"listen"`
	Port            int         `json:"port"`
	FailureMode     string      `json:"failureMode"`
	Primary         DNSUpstream `json:"primary"`
	Fallback        DNSUpstream `json:"fallback"`
	RestrictClients bool        `json:"restrictClients"`
	AllowedNetworks []string    `json:"allowedNetworks"`
	CacheSize       int         `json:"cacheSize"`
	AutoWireGuard   bool        `json:"autoWireGuard"`
	ClientDNS       string      `json:"clientDns"`
}

func defaultDNSSettings() DNSSettings {
	return DNSSettings{Listen: "0.0.0.0", Port: 53, FailureMode: "block", Primary: DNSUpstream{Protocol: "doh", Address: "https://cloudflare-dns.com/dns-query"}, Fallback: DNSUpstream{Protocol: "doh", Address: "https://dns.google/dns-query"}, CacheSize: 1024, AutoWireGuard: true, ClientDNS: "1.1.1.1", AllowedNetworks: []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}}
}
func (d DNSSettings) validate() error {
	if net.ParseIP(d.Listen) == nil || d.Port < 1 || d.Port > 65535 {
		return errors.New("DNS: укажите IP прослушивания и порт от 1 до 65535")
	}
	if d.FailureMode != "block" && d.FailureMode != "fallback" {
		return errors.New("DNS: неизвестное поведение при сбое")
	}
	if d.CacheSize < 0 || d.CacheSize > 10000 {
		return errors.New("DNS: размер кеша должен быть от 0 до 10000")
	}
	if err := d.Primary.validate(); err != nil {
		return fmt.Errorf("основной DNS: %w", err)
	}
	if d.FailureMode == "fallback" {
		if err := d.Fallback.validate(); err != nil {
			return fmt.Errorf("резервный DNS: %w", err)
		}
	}
	if len(d.AllowedNetworks) > 100 {
		return errors.New("DNS: максимум 100 подсетей")
	}
	for _, n := range d.AllowedNetworks {
		if _, err := netip.ParsePrefix(n); err != nil {
			return fmt.Errorf("DNS: некорректная подсеть %s", n)
		}
	}
	for _, v := range strings.Split(d.ClientDNS, ",") {
		if strings.TrimSpace(v) != "" && net.ParseIP(strings.TrimSpace(v)) == nil {
			return errors.New("DNS клиентов: укажите IP-адреса через запятую")
		}
	}
	return nil
}
func (u DNSUpstream) endpoint() (host, port string, err error) {
	if u.Protocol == "doh" {
		var v *url.URL
		v, err = url.Parse(u.Address)
		if err != nil || v.Scheme != "https" || v.Hostname() == "" || v.User != nil || v.Fragment != "" {
			return "", "", errors.New("нужен HTTPS URL без логина и фрагмента")
		}
		host, port = v.Hostname(), v.Port()
		if port == "" {
			port = "443"
		}
		return
	}
	port = "53"
	if u.Protocol == "dot" {
		port = "853"
	}
	host = u.Address
	if h, p, e := net.SplitHostPort(u.Address); e == nil {
		host, port = h, p
	} else if net.ParseIP(host) == nil && strings.Contains(host, ":") {
		return "", "", errors.New("некорректный адрес сервера")
	}
	return
}
func (u DNSUpstream) validate() error {
	switch u.Protocol {
	case "udp", "tcp", "doh", "dot":
	default:
		return errors.New("протокол должен быть UDP, TCP, DoH или DoT")
	}
	host, port, err := u.endpoint()
	if err != nil {
		return err
	}
	p, e := strconv.Atoi(port)
	if e != nil || p < 1 || p > 65535 || host == "" || strings.ContainsAny(host, " /\r\n\t") {
		return errors.New("некорректный адрес или порт DNS")
	}
	if u.IP != "" && net.ParseIP(u.IP) == nil {
		return errors.New("IP подключения должен быть IP-адресом")
	}
	if strings.ContainsAny(u.TLSName, " /:\r\n\t") {
		return errors.New("некорректный TLS-домен")
	}
	return nil
}
