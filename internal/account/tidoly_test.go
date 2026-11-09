package account

import (
	"encoding/json"
	"testing"
)

// Home writes the tunnel server as frpc's own settings; "host:port" is taken too.
func TestTheTunnelServerAsHomeWritesIt(t *testing.T) {
	for in, want := range map[string]string{
		`{"serverAddr":"stow-tunnel.tidoly.app","serverPort":7001}`: "stow-tunnel.tidoly.app:7001",
		`"stow-tunnel.tidoly.app:7001"`:                             "stow-tunnel.tidoly.app:7001",
		`{"serverAddr":"stow-tunnel.tidoly.app"}`:                   "stow-tunnel.tidoly.app",
		`null`: "",
		`{}`:   "",
	} {
		var a frpsAddress
		if err := json.Unmarshal([]byte(in), &a); err != nil || string(a) != want {
			t.Errorf("%s: %q %v, want %q", in, a, err, want)
		}
	}
}
