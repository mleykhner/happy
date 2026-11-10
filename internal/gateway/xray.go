package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Xray checks the file extension even when -format=json is provided.
const stagedConfigName = "xray.next.json"

type Xray struct {
	mu          sync.Mutex
	binary, dir string
	cmd         *exec.Cmd
	done        chan struct{}
	last        []byte
	metricsPort int
	generation  uint64
	probePort   int
	probeSecret string
}

func NewXray(binary, dir string) *Xray {
	return &Xray{binary: binary, dir: dir, probeSecret: randomToken()}
}
func (x *Xray) Running() bool { x.mu.Lock(); defer x.mu.Unlock(); return x.running() }
func (x *Xray) running() bool {
	if x.cmd == nil {
		return false
	}
	select {
	case <-x.done:
		return false
	default:
		return true
	}
}
func BuildConfig(s State, probePort int, probeSecret string) ([]byte, error) {
	return buildConfig(s, probePort, probeSecret, 0)
}

func buildConfig(s State, probePort int, probeSecret string, metricsPort int) ([]byte, error) {
	block := map[string]any{"tag": "block", "protocol": "blackhole", "settings": map[string]any{}}
	outbounds := []any{block}
	target := "block"
	if n := s.activeNode(); n != nil && !s.ExitDisabled {
		outbounds = append(outbounds, n.Outbound)
		target = "proxy"
	}
	inbounds := []any{map[string]any{"tag": "health", "listen": "127.0.0.1", "port": probePort, "protocol": "http", "settings": map[string]any{"accounts": []any{map[string]any{"user": "happy", "pass": probeSecret}}}}}
	if s.Client != nil && s.Client.Enabled {
		inbounds = append(inbounds, map[string]any{"tag": "wireguard", "listen": "0.0.0.0", "port": s.Settings.WireGuardPort, "protocol": "wireguard", "settings": map[string]any{"secretKey": s.ServerPrivate, "mtu": 1280, "peers": []any{map[string]any{"publicKey": s.Client.PublicKey, "allowedIPs": []string{"10.77.0.2/32", "fd77::2/128"}}}}})
	}
	blocked := []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "::/0"}
	if ip := net.ParseIP(s.Settings.Endpoint); ip != nil {
		if ip.To4() != nil {
			blocked = append(blocked, ip.String()+"/32")
		}
	}
	// Pin blocking of this host's interface addresses as well as private networks.
	if addresses, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addresses {
			ip, _, e := net.ParseCIDR(a.String())
			if e == nil && ip.To4() != nil {
				blocked = append(blocked, ip.String()+"/32")
			}
		}
	}
	rules := []any{map[string]any{"type": "field", "inboundTag": []string{"wireguard"}, "ip": blocked, "outboundTag": "block"}, map[string]any{"type": "field", "network": "tcp,udp", "outboundTag": target}}
	config := map[string]any{"log": map[string]any{"loglevel": "none"}, "inbounds": inbounds, "outbounds": outbounds, "routing": map[string]any{"domainStrategy": "AsIs", "rules": rules}}
	if metricsPort > 0 {
		config["metrics"] = map[string]any{"listen": fmt.Sprintf("127.0.0.1:%d", metricsPort)}
		config["stats"] = map[string]any{}
		config["policy"] = map[string]any{"system": map[string]any{"statsInboundUplink": true, "statsInboundDownlink": true}}
	}
	return json.MarshalIndent(config, "", "  ")
}
func (x *Xray) Apply(ctx context.Context, s State, forceClosed bool) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.probePort == 0 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		x.probePort = l.Addr().(*net.TCPAddr).Port
		l.Close()
	}
	if x.metricsPort == 0 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		x.metricsPort = l.Addr().(*net.TCPAddr).Port
		l.Close()
	}
	b, err := buildConfig(s, x.probePort, x.probeSecret, x.metricsPort)
	if err != nil {
		return err
	}
	path := filepath.Join(x.dir, "xray.json")
	pending := filepath.Join(x.dir, stagedConfigName)
	if err = os.WriteFile(pending, b, 0600); err != nil {
		return err
	}
	defer os.Remove(pending)
	testCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err = x.validate(testCtx, pending); err != nil {
		if forceClosed {
			x.stop()
		}
		return errors.New("Xray отклонил конфигурацию; проверьте совместимость узла и версию ядра")
	}
	previous := x.last
	x.stop()
	if err = os.Rename(pending, path); err == nil {
		err = x.start(path)
	}
	if err != nil {
		if !forceClosed && len(previous) > 0 {
			if os.WriteFile(path, previous, 0600) == nil {
				if rollbackErr := x.start(path); rollbackErr != nil {
					return errors.New("не удалось запустить Xray и восстановить предыдущую конфигурацию; выход заблокирован")
				}
			}
		}
		return errors.New("не удалось запустить Xray; новая конфигурация не применена")
	}
	x.last = b
	return nil
}

// validate is also exercised against the actual core with staged configuration paths.
func (x *Xray) validate(ctx context.Context, path string) error {
	return exec.CommandContext(ctx, x.binary, "run", "-test", "-config", path).Run()
}
func (x *Xray) start(path string) error {
	cmd := exec.Command(x.binary, "run", "-config", path)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}
	x.generation++
	x.cmd = cmd
	x.done = make(chan struct{})
	done := x.done
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
		return errors.New("Xray завершился при запуске")
	case <-time.After(350 * time.Millisecond):
		return nil
	}
}
func (x *Xray) stop() {
	if !x.running() {
		x.cmd = nil
		return
	}
	_ = x.cmd.Process.Signal(os.Interrupt)
	select {
	case <-x.done:
	case <-time.After(3 * time.Second):
		_ = x.cmd.Process.Kill()
		<-x.done
	}
	x.cmd = nil
}
func (x *Xray) Stop() { x.mu.Lock(); defer x.mu.Unlock(); x.stop() }
func (x *Xray) Probe(ctx context.Context) (time.Duration, error) {
	x.mu.Lock()
	port, secret, running := x.probePort, x.probeSecret, x.running()
	x.mu.Unlock()
	if !running {
		return 0, errors.New("Xray не запущен")
	}
	proxy := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", port), User: url.UserPassword("happy", secret)}
	tr := &http.Transport{Proxy: http.ProxyURL(proxy), TLSHandshakeTimeout: 8 * time.Second}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://www.gstatic.com/generate_204", nil)
	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return 0, errors.New("выбранный узел не прошёл проверку HTTPS")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		return 0, errors.New("неожиданный ответ проверки HTTPS")
	}
	return time.Since(start), nil
}
