package network

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"

	"github.com/InfrastryAI/infra/internal/api"
)

func TestValidateRestrictsPrivilegedConfiguration(t *testing.T) {
	key, _ := ecdh.X25519().GenerateKey(rand.Reader)
	config := Configuration{PrivateKey: base64.StdEncoding.EncodeToString(key.Bytes()), Session: api.NetworkSession{ID: "session", Mode: "native", Source: "fd42:f1a5:6e74:2::1", DNS: dnsAddress, DNSSuffix: "app.team.internal", MTU: 1280, ExpiresAt: time.Now().Add(time.Hour), Services: []api.NetworkService{{Name: "api", Address: "fd42:f1a5:6e74:1::1", Hostname: "api.app.team.internal"}}}}
	config.Session.Gateway.Endpoint = "127.0.0.1:51820"
	config.Session.Gateway.PublicKey = base64.StdEncoding.EncodeToString(key.PublicKey().Bytes())
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Configuration){
		"source":             func(c *Configuration) { c.Session.Source = "192.168.1.1" },
		"dns":                func(c *Configuration) { c.Session.DNS = "8.8.8.8" },
		"suffix":             func(c *Configuration) { c.Session.DNSSuffix = "../../etc" },
		"expiry":             func(c *Configuration) { c.Session.ExpiresAt = time.Now().Add(-time.Second) },
		"endpoint injection": func(c *Configuration) { c.Session.Gateway.Endpoint = "127.0.0.1:5\nallowed_ip=::/0" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := config
			mutate(&changed)
			if changed.Validate() == nil {
				t.Fatal("unsafe helper configuration accepted")
			}
		})
	}
}
