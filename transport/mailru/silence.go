package mailru

import (
	"encoding/json"
	"time"
)

// engineIOSilence is how long a connection may hear nothing from the server
// before it counts as dead: the server pings every pingInterval and waits
// pingTimeout for the answer, so a live connection hears it at least that
// often. Without a limit the reader waited on a socket the network had
// dropped (a phone moving from Wi-Fi to mobile data, a NAT forgetting it)
// until TCP gave up, minutes later, with the tunnel down all along.
func engineIOSilence(open string) time.Duration {
	var o struct {
		PingInterval int `json:"pingInterval"`
		PingTimeout  int `json:"pingTimeout"`
	}
	if json.Unmarshal([]byte(open), &o) != nil || o.PingInterval <= 0 {
		return defaultSilence
	}
	return time.Duration(o.PingInterval+max(o.PingTimeout, 0))*time.Millisecond + silenceSlack
}

// defaultSilence: Engine.IO's defaults (25 s + 20 s) and some slack.
const defaultSilence = 50 * time.Second

// silenceSlack covers a ping delayed on its way; a variable for tests.
var silenceSlack = 5 * time.Second
