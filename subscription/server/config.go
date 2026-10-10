// Package server is the subscription service (opensub): accounts, devices,
// plans, orders and the list of locations a subscriber may connect through.
// See docs/subscriptions.ru.md for the whole design.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
)

// Config is /etc/openflux-sub/config.json.
type Config struct {
	// Listen is the address the HTTPS service listens on, ":8443".
	Listen string `json:"listen"`
	// PublicHost is the address clients reach the service at (IP or domain); it
	// goes into the certificate and into keys.
	PublicHost string `json:"publicHost"`
	// AdminToken guards /admin; the opensub commands read it from here.
	AdminToken string `json:"adminToken"`
	// DataDir holds state.json and, for a self-signed certificate, cert.pem and key.pem.
	DataDir string `json:"dataDir"`
	// CertFile and KeyFile are a real certificate (a domain); empty makes a
	// self-signed one in DataDir, which apps trust by its pin.
	CertFile string `json:"certFile,omitempty"`
	KeyFile  string `json:"keyFile,omitempty"`
	// NodeConfDir is where node-install.sh keeps its channels.
	NodeConfDir string `json:"nodeConfDir,omitempty"`

	Title       string `json:"title"`
	Support     string `json:"support,omitempty"`
	Announce    string `json:"announce,omitempty"`
	UpdateHours int    `json:"updateHours,omitempty"`
	// AutoTrial gives a new account the trial plan at once.
	AutoTrial bool `json:"autoTrial"`

	Pools []Pool `json:"pools"`
	Plans []Plan `json:"plans"`
	// Payment describes how people pay; "manual" waits for the owner to confirm.
	Payment Payment `json:"payment"`

	// BcryptCost is for tests; 0 means 12.
	BcryptCost int `json:"bcryptCost,omitempty"`
}

// Pool is one location: a channel of a node installed by node-install.sh.
type Pool struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Channel string `json:"channel"`
	// Host is the node's address in the link's direct carrier (default PublicHost).
	Host string `json:"host,omitempty"`
}

// Plan is what a subscriber can have.
type Plan struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Days    int    `json:"days"`
	Devices int    `json:"devices"`
	// Price is shown as is ("299 ₽"); the service does not bill.
	Price string `json:"price,omitempty"`
	// Trial plans are not for sale: a new account gets one once.
	Trial bool `json:"trial,omitempty"`
	// Pools limits the plan's locations (ids); empty is all.
	Pools []string `json:"pools,omitempty"`
}

// Payment is how orders are paid.
type Payment struct {
	Provider     string `json:"provider"`
	Instructions string `json:"instructions,omitempty"`
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// LoadConfig reads and checks the config file.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, c.Check()
}

// Check validates the config and fills the defaults.
func (c *Config) Check() error {
	if c.Listen == "" {
		c.Listen = ":8443"
	}
	if c.PublicHost == "" {
		return errors.New("config: publicHost is required")
	}
	if len(c.AdminToken) < 24 {
		return errors.New("config: adminToken must be at least 24 characters")
	}
	if c.DataDir == "" {
		c.DataDir = "/var/lib/openflux-sub"
	}
	if c.NodeConfDir == "" {
		c.NodeConfDir = "/etc/openflux-node"
	}
	if c.Title == "" {
		c.Title = "OpenFlux"
	}
	if c.UpdateHours <= 0 {
		c.UpdateHours = 6
	}
	if c.Payment.Provider == "" {
		c.Payment.Provider = "manual"
	}
	if c.Payment.Provider != "manual" {
		return fmt.Errorf("config: payment provider %q is not available (manual only)", c.Payment.Provider)
	}
	if len(c.Pools) == 0 {
		return errors.New("config: at least one pool is required")
	}
	pools := map[string]bool{}
	for i := range c.Pools {
		p := &c.Pools[i]
		if !idRe.MatchString(p.ID) || p.Name == "" || p.Channel == "" || !idRe.MatchString(p.Channel) {
			return fmt.Errorf("config: pool %q: id, name and channel (a-z0-9-_) are required", p.ID)
		}
		if pools[p.ID] {
			return fmt.Errorf("config: pool %q twice", p.ID)
		}
		pools[p.ID] = true
		if p.Host == "" {
			p.Host = c.PublicHost
		}
	}
	if len(c.Plans) == 0 {
		return errors.New("config: at least one plan is required")
	}
	plans := map[string]bool{}
	for _, p := range c.Plans {
		if !idRe.MatchString(p.ID) || p.Name == "" || p.Days < 1 || p.Devices < 1 {
			return fmt.Errorf("config: plan %q: id, name, days and devices are required", p.ID)
		}
		if plans[p.ID] {
			return fmt.Errorf("config: plan %q twice", p.ID)
		}
		plans[p.ID] = true
		for _, id := range p.Pools {
			if !pools[id] {
				return fmt.Errorf("config: plan %q: unknown pool %q", p.ID, id)
			}
		}
	}
	return nil
}

func (c *Config) plan(id string) (Plan, bool) {
	for _, p := range c.Plans {
		if p.ID == id {
			return p, true
		}
	}
	return Plan{}, false
}

func (c *Config) trialPlan() (Plan, bool) {
	for _, p := range c.Plans {
		if p.Trial {
			return p, true
		}
	}
	return Plan{}, false
}
