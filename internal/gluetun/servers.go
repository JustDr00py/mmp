package gluetun

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// ServerList reads gluetun's servers.json (gluetun writes it to /gluetun
// on start; the VPN compose file shares that directory with MMP) for the
// countries, regions and cities to offer. Without it the location is
// typed in, and gluetun still checks it.
type ServerList struct {
	Path string

	mu    sync.Mutex
	stamp time.Time
	size  int64
	cache map[string]Choices
}

// Choices are the locations a provider's servers offer for a VPN type.
type Choices struct {
	Countries []Country
	// Regions are offered only for providers whose servers name no
	// country (Private Internet Access's "AU Adelaide"); elsewhere they
	// are continents.
	Regions []string
	Cities  []City // by country, then name
}

// Country is a country gluetun has servers in.
type Country struct {
	Name, Flag string
	Servers    int
}

// City is a city gluetun has servers in.
type City struct {
	Name, Country string
	Servers       int
}

// Empty reports whether nothing can be offered (no list, or an unknown
// provider).
func (c Choices) Empty() bool { return len(c.Countries)+len(c.Regions)+len(c.Cities) == 0 }

// HasCountry reports whether name is one of the countries (any case).
func (c Choices) HasCountry(name string) bool {
	return slices.ContainsFunc(c.Countries, func(x Country) bool { return strings.EqualFold(x.Name, name) })
}

// Choices lists provider's locations for vpnType (wireguard or openvpn).
// A missing file gives no choices and no error.
func (l *ServerList) Choices(provider, vpnType string) (Choices, error) {
	if l == nil || l.Path == "" {
		return Choices{}, nil
	}
	fi, err := os.Stat(l.Path)
	if os.IsNotExist(err) {
		return Choices{}, nil
	} else if err != nil {
		return Choices{}, err
	}
	key := strings.ToLower(provider) + "\x00" + strings.ToLower(vpnType)
	l.mu.Lock()
	defer l.mu.Unlock()
	if !fi.ModTime().Equal(l.stamp) || fi.Size() != l.size {
		l.cache, l.stamp, l.size = map[string]Choices{}, fi.ModTime(), fi.Size()
	}
	if c, ok := l.cache[key]; ok {
		return c, nil
	}
	c, err := readChoices(l.Path, provider, vpnType)
	if err != nil {
		return Choices{}, err
	}
	l.cache[key] = c
	return c, nil
}

type server struct {
	VPN     string `json:"vpn"`
	Country string `json:"country"`
	Region  string `json:"region"`
	City    string `json:"city"`
}

func readChoices(path, provider, vpnType string) (Choices, error) {
	f, err := os.Open(path)
	if err != nil {
		return Choices{}, err
	}
	defer f.Close()
	var all map[string]json.RawMessage
	if err := json.NewDecoder(f).Decode(&all); err != nil {
		return Choices{}, fmt.Errorf("%s: %w", path, err)
	}
	var raw json.RawMessage
	for name, v := range all {
		if strings.EqualFold(name, provider) {
			raw = v
		}
	}
	if raw == nil {
		return Choices{}, nil
	}
	var p struct {
		Servers []server `json:"servers"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return Choices{}, fmt.Errorf("%s: %s: %w", path, provider, err)
	}
	return choicesOf(p.Servers, vpnType), nil
}

// choicesOf collects the locations of the servers for vpnType (an
// OpenVPN server may leave vpn empty). If none match, all servers count.
func choicesOf(servers []server, vpnType string) Choices {
	match := func(s server) bool {
		return strings.EqualFold(s.VPN, vpnType) || (s.VPN == "" && strings.EqualFold(vpnType, "openvpn"))
	}
	if !slices.ContainsFunc(servers, match) {
		match = func(server) bool { return true }
	}
	countries, regions, cities := map[string]int{}, map[string]int{}, map[[2]string]int{}
	for _, s := range servers {
		if !match(s) {
			continue
		}
		if s.Country != "" {
			countries[s.Country]++
		}
		if s.Region != "" {
			regions[s.Region]++
		}
		if s.City != "" {
			cities[[2]string{s.Country, s.City}]++
		}
	}
	var c Choices
	for name, n := range countries {
		c.Countries = append(c.Countries, Country{Name: name, Flag: Flag(name), Servers: n})
	}
	slices.SortFunc(c.Countries, func(a, b Country) int { return strings.Compare(a.Name, b.Name) })
	if len(countries) == 0 {
		for name := range regions {
			c.Regions = append(c.Regions, name)
		}
		slices.Sort(c.Regions)
	}
	for k, n := range cities {
		c.Cities = append(c.Cities, City{Country: k[0], Name: k[1], Servers: n})
	}
	slices.SortFunc(c.Cities, func(a, b City) int {
		if r := strings.Compare(a.Country, b.Country); r != 0 {
			return r
		}
		return strings.Compare(a.Name, b.Name)
	})
	return c
}
