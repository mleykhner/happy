package gateway

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

type Node struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Protocol string         `json:"protocol"`
	Host     string         `json:"host"`
	Port     int            `json:"port"`
	Outbound map[string]any `json:"outbound"`
}
type Subscription struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Nodes     []Node    `json:"nodes"`
	Updated   time.Time `json:"updated"`
	Attempted time.Time `json:"attempted"`
	Error     string    `json:"error"`
}
type Client struct {
	PrivateKey string `json:"privateKey"`
	PublicKey  string `json:"publicKey"`
	Enabled    bool   `json:"enabled"`
}
type Settings struct {
	Endpoint       string `json:"endpoint"`
	WireGuardPort  int    `json:"wireguardPort"`
	RefreshMinutes int    `json:"refreshMinutes"`
}
type State struct {
	DeviceID      string         `json:"deviceId"`
	Settings      Settings       `json:"settings"`
	Subscriptions []Subscription `json:"subscriptions"`
	Selected      string         `json:"selected"`
	ServerPrivate string         `json:"serverPrivate"`
	ServerPublic  string         `json:"serverPublic"`
	Client        *Client        `json:"client"`
}

func initialState() (State, error) {
	priv, pub, err := keyPair()
	return State{DeviceID: newDeviceID(), Settings: Settings{WireGuardPort: 51820, RefreshMinutes: 60}, Subscriptions: []Subscription{}, ServerPrivate: priv, ServerPublic: pub}, err
}
func keyPair() (string, string, error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	b := k.Bytes()
	b[0] &= 248
	b[31] &= 127
	b[31] |= 64
	return base64.StdEncoding.EncodeToString(b), base64.StdEncoding.EncodeToString(k.PublicKey().Bytes()), nil
}
func randomToken() string      { return rand.Text() }
func identity(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:16]) }
func (s State) activeNode() *Node {
	for _, sub := range s.Subscriptions {
		for _, n := range sub.Nodes {
			if n.ID == s.Selected {
				return &n
			}
		}
	}
	return nil
}
func (s Settings) validate() error {
	if s.WireGuardPort < 1024 || s.WireGuardPort > 65535 {
		return errors.New("порт WireGuard должен быть от 1024 до 65535")
	}
	if s.RefreshMinutes < 5 || s.RefreshMinutes > 10080 {
		return errors.New("интервал обновления должен быть от 5 до 10080 минут")
	}
	if s.Endpoint != "" {
		if strings.ContainsAny(s.Endpoint, "\r\n\t /#[]=;") || strings.Contains(s.Endpoint, ":") && net.ParseIP(s.Endpoint) == nil {
			return errors.New("укажите IP или имя сервера без порта")
		}
		if net.ParseIP(s.Endpoint) == nil {
			for _, r := range s.Endpoint {
				if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-') {
					return errors.New("некорректное имя сервера")
				}
			}
		}
	}
	return nil
}
func (s State) clientConfig() (string, error) {
	if s.Client == nil {
		return "", errors.New("клиент ещё не создан")
	}
	if s.Settings.Endpoint == "" {
		return "", errors.New("укажите внешний адрес сервера в настройках")
	}
	return fmt.Sprintf("[Interface]\nPrivateKey = %s\nAddress = 10.77.0.2/32, fd77::2/128\nDNS = 1.1.1.1\nMTU = 1280\n\n[Peer]\nPublicKey = %s\nEndpoint = %s\nAllowedIPs = 0.0.0.0/0, ::/0\nPersistentKeepalive = 25\n", s.Client.PrivateKey, s.ServerPublic, net.JoinHostPort(s.Settings.Endpoint, strconv.Itoa(s.Settings.WireGuardPort))), nil
}

// Use an installation identity rather than exposing host hardware identifiers.
// The UUID is persisted in SQLite and remains stable across updates and restarts.
func newDeviceID() string {
	b := sha256.Sum256([]byte(rand.Text()))
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%X-%X-%X-%X-%X", b[:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
