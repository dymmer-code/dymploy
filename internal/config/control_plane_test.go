package config

import "testing"

func TestControlPlaneRequiresPinnedTLSAndPrivateTokenPath(t *testing.T) {
	good := ControlPlaneConfig{URL: "wss://dymmer.test/builders/websocket", BuilderID: "builder", TokenFile: "/etc/dymploy/token", SSHKeys: map[string]string{"app": "/etc/key"}}
	if err := good.validate(); err != nil {
		t.Fatal(err)
	}
	for _, url := range []string{"ws://dymmer.test/builders/websocket", "wss://user:secret@dymmer.test/builders/websocket", "wss://dymmer.test/other", "wss://dymmer.test/builders/websocket?token=x", "wss://dymmer.test/builders/websocket#x"} {
		c := good
		c.URL = url
		if c.validate() == nil {
			t.Fatal(url)
		}
	}
	for _, change := range []func(*ControlPlaneConfig){func(c *ControlPlaneConfig) { c.URL = "" }, func(c *ControlPlaneConfig) { c.TokenFile = "relative" }, func(c *ControlPlaneConfig) { c.BuilderID = "" }, func(c *ControlPlaneConfig) { c.SSHKeys = map[string]string{"bad": "relative"} }} {
		c := good
		change(&c)
		if c.validate() == nil {
			t.Fatal(c)
		}
	}
}
