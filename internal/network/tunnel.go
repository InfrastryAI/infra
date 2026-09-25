package network

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/InfrastryAI/infra/internal/api"
	"github.com/zalando/go-keyring"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

var servicesPrefix = netip.MustParsePrefix("fd42:f1a5:6e74:1::/64")
var peersPrefix = netip.MustParsePrefix("fd42:f1a5:6e74:2::/64")

const dnsAddress = "fd42:f1a5:6e74:3::53"

var suffixPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}\.[a-z0-9][a-z0-9-]{0,62}\.internal$`)

type Configuration struct {
	Session    api.NetworkSession `json:"session"`
	PrivateKey string             `json:"private_key"`
}

func ForgetDeviceKey(apiURL, team, mode string) error {
	digest := sha256.Sum256([]byte(apiURL + "\x00" + team + "\x00" + mode))
	err := keyring.Delete("infrastry-network", hex.EncodeToString(digest[:]))
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	if err != nil {
		return errors.New("cannot remove the device key; unlock your system keychain and retry")
	}
	return nil
}

func DeviceKey(apiURL, team, mode string) (string, string, error) {
	digest := sha256.Sum256([]byte(apiURL + "\x00" + team + "\x00" + mode))
	account := hex.EncodeToString(digest[:])
	encoded, err := keyring.Get("infrastry-network", account)
	if errors.Is(err, keyring.ErrNotFound) {
		key, generateErr := ecdh.X25519().GenerateKey(rand.Reader)
		if generateErr != nil {
			return "", "", generateErr
		}
		encoded = base64.StdEncoding.EncodeToString(key.Bytes())
		if err = keyring.Set("infrastry-network", account, encoded); err != nil {
			return "", "", errors.New("cannot securely store the device key; unlock your system keychain and retry")
		}
	} else if err != nil {
		return "", "", errors.New("cannot read the device key; unlock your system keychain and retry")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", errors.New("invalid stored device key")
	}
	key, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return "", "", errors.New("invalid stored device key")
	}
	return encoded, base64.StdEncoding.EncodeToString(key.PublicKey().Bytes()), nil
}

func (c Configuration) Validate() error {
	s := c.Session
	source, err := netip.ParseAddr(s.Source)
	if err != nil || !peersPrefix.Contains(source) || s.MTU != 1280 || s.DNS != dnsAddress || !suffixPattern.MatchString(s.DNSSuffix) || s.ID == "" || !s.ExpiresAt.After(time.Now()) || s.ExpiresAt.After(time.Now().Add(time.Hour+time.Minute)) || len(s.Services) == 0 || len(s.Services) > 128 {
		return errors.New("invalid private network session")
	}
	for _, key := range []string{c.PrivateKey, s.Gateway.PublicKey} {
		raw, err := base64.StdEncoding.DecodeString(key)
		if err != nil || len(raw) != 32 {
			return errors.New("invalid WireGuard key")
		}
	}
	endpoint, err := netip.ParseAddrPort(s.Gateway.Endpoint)
	if err != nil || endpoint.Port() == 0 || endpoint.Addr().IsUnspecified() || endpoint.Addr().IsMulticast() {
		return errors.New("gateway must have a valid IP endpoint")
	}
	for _, service := range s.Services {
		address, err := netip.ParseAddr(service.Address)
		if err != nil || !servicesPrefix.Contains(address) || service.Hostname != service.Name+"."+s.DNSSuffix {
			return errors.New("invalid private service address")
		}
	}
	return nil
}

func wireguard(c Configuration, t tun.Device) (*device.Device, error) {
	private, _ := base64.StdEncoding.DecodeString(c.PrivateKey)
	public, _ := base64.StdEncoding.DecodeString(c.Session.Gateway.PublicKey)
	var config strings.Builder
	fmt.Fprintf(&config, "private_key=%s\npublic_key=%s\nendpoint=%s\npersistent_keepalive_interval=20\nallowed_ip=%s/128\n", hex.EncodeToString(private), hex.EncodeToString(public), c.Session.Gateway.Endpoint, dnsAddress)
	for _, s := range c.Session.Services {
		fmt.Fprintf(&config, "allowed_ip=%s/128\n", s.Address)
	}
	dev := device.NewDevice(t, conn.NewDefaultBind(), device.NewLogger(device.LogLevelSilent, ""))
	if err := dev.IpcSet(config.String()); err != nil {
		dev.Close()
		return nil, errors.New("could not configure private connection")
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, errors.New("could not start private connection")
	}
	return dev, nil
}

type Tunnel struct {
	device  *device.Device
	Network *netstack.Net
}

func Open(c Configuration) (*Tunnel, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	source := netip.MustParseAddr(c.Session.Source)
	t, network, err := netstack.CreateNetTUN([]netip.Addr{source}, []netip.Addr{netip.MustParseAddr(dnsAddress)}, 1280)
	if err != nil {
		return nil, err
	}
	dev, err := wireguard(c, t)
	if err != nil {
		return nil, err
	}
	return &Tunnel{device: dev, Network: network}, nil
}
func (t *Tunnel) Close() { t.device.Close() }

// Forward binds explicitly to loopback, preserving independent TCP connections
// for GUI pools. Cancellation closes the listener and every active stream.
func (t *Tunnel) Forward(ctx context.Context, listener net.Listener, destination string) error {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	var mu sync.Mutex
	connections := map[net.Conn]bool{}
	done := make(chan struct{})
	defer func() { cancel(); <-done; workers.Wait() }()
	go func() {
		defer close(done)
		<-ctx.Done()
		listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for c := range connections {
			c.Close()
		}
	}()
	for {
		local, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		mu.Lock()
		if ctx.Err() != nil || len(connections) >= 128 {
			mu.Unlock()
			local.Close()
			continue
		}
		connections[local] = true
		mu.Unlock()
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer local.Close()
			defer func() { mu.Lock(); delete(connections, local); mu.Unlock() }()
			dialCtx, cancelDial := context.WithTimeout(ctx, 5*time.Second)
			upstream, err := t.Network.DialContext(dialCtx, "tcp", destination)
			cancelDial()
			if err != nil {
				return
			}
			defer upstream.Close()
			mu.Lock()
			if ctx.Err() != nil {
				mu.Unlock()
				return
			}
			connections[upstream] = true
			mu.Unlock()
			defer func() { mu.Lock(); delete(connections, upstream); mu.Unlock() }()
			copied := make(chan struct{})
			go func() { io.Copy(upstream, local); upstream.Close(); local.Close(); close(copied) }()
			io.Copy(local, upstream)
			local.Close()
			upstream.Close()
			<-copied
		}()
	}
}
