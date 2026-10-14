// Package xtream is a client for provider-side Xtream Codes panel APIs:
// authentication, live categories, live streams, and stream URL building.
package xtream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sysadmin/mmp/internal/redact"
)

// ErrUnauthorized is returned when the panel rejects the credentials.
var ErrUnauthorized = errors.New("xtream: invalid credentials")

// Client talks to one Xtream panel.
type Client struct {
	base     *url.URL
	username string
	password string
	ua       string
	hc       *http.Client
}

// New builds a client for the panel at baseURL (e.g. http://host:8080).
func New(baseURL, username, password, userAgent string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("xtream: invalid base URL %q", redact.URL(baseURL))
	}
	hc := &http.Client{} // timeouts come from the caller's context
	return &Client{base: u, username: username, password: password, ua: userAgent, hc: hc}, nil
}

// AuthResponse is the action-less player_api.php payload.
type AuthResponse struct {
	UserInfo   UserInfo   `json:"user_info"`
	ServerInfo ServerInfo `json:"server_info"`
}

// UserInfo mirrors the panel fields MMP cares about. Panels send numbers
// as strings, so strings are used throughout.
type UserInfo struct {
	Username       string `json:"username"`
	Auth           int    `json:"auth"`
	Status         string `json:"status"`
	ExpDate        string `json:"exp_date"`
	MaxConnections string `json:"max_connections"`
	ActiveCons     string `json:"active_cons"`
	CreatedAt      string `json:"created_at"`
	IsTrial        string `json:"is_trial"`
}

// ServerInfo describes how to reach the panel's media.
type ServerInfo struct {
	URL            string `json:"url"`
	Port           string `json:"port"`
	HTTPSPort      string `json:"https_port"`
	ServerProtocol string `json:"server_protocol"`
	Timezone       string `json:"timezone"`
}

// Login performs the action-less player_api.php call and verifies that the
// panel accepted the credentials.
func (c *Client) Login(ctx context.Context) (*AuthResponse, error) {
	var out AuthResponse
	if err := c.get(ctx, c.apiURL(""), &out); err != nil {
		return nil, err
	}
	if out.UserInfo.Auth != 1 {
		return nil, ErrUnauthorized
	}
	return &out, nil
}

// Category is one live category from the panel.
type Category struct {
	CategoryName string `json:"category_name"`
	CategoryID   string `json:"category_id"`
	ParentID     int    `json:"parent_id"`
}

// LiveCategories fetches the provider's live categories.
func (c *Client) LiveCategories(ctx context.Context) ([]Category, error) {
	var out []Category
	if err := c.get(ctx, c.apiURL("get_live_categories"), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Stream is one live stream from the panel.
type Stream struct {
	Num        int    `json:"num"`
	Name       string `json:"name"`
	StreamType string `json:"stream_type"`
	StreamID   int64  `json:"stream_id"`
	StreamIcon string `json:"stream_icon"`
	EPGChannel string `json:"epg_channel_id"` // maps to XMLTV channel id
	Added      string `json:"added"`
	CategoryID string `json:"category_id"`
	CustomSid  string `json:"custom_sid"`
	TVFormat   string `json:"tv_format"`
}

// LiveStreams fetches the provider's live streams.
func (c *Client) LiveStreams(ctx context.Context) ([]Stream, error) {
	var out []Stream
	if err := c.get(ctx, c.apiURL("get_live_streams"), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// StreamURL builds the direct playback URL for a live stream.
// Credentials are embedded in the path: callers must redact before logging.
func (c *Client) StreamURL(streamID int64) string {
	return fmt.Sprintf("%s/live/%s/%s/%d.ts", c.base.String(),
		url.PathEscape(c.username), url.PathEscape(c.password), streamID)
}

// XMLTVURL builds the panel's XMLTV EPG URL.
func (c *Client) XMLTVURL() string {
	return c.base.String() + "/xmltv.php?username=" + url.QueryEscape(c.username) +
		"&password=" + url.QueryEscape(c.password)
}

// apiURL builds a player_api.php URL; action may be empty.
func (c *Client) apiURL(action string) string {
	q := url.Values{}
	q.Set("username", c.username)
	q.Set("password", c.password)
	if action != "" {
		q.Set("action", action)
	}
	return c.base.String() + "/player_api.php?" + q.Encode()
}

// maxBody bounds response sizes so a broken panel can't exhaust memory.
const maxBody = 256 << 20 // 256 MiB: stream lists get large

func (c *Client) get(ctx context.Context, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return errors.New("xtream: build request: invalid url") // err echoes the URL, password included
	}
	if c.ua != "" {
		req.Header.Set("User-Agent", c.ua)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("xtream: request: %w", redact.Err(err)) // the URL carries the password
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("xtream: status %d", resp.StatusCode)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxBody))
	if err := dec.Decode(out); err != nil {
		// Panels sometimes answer plain-text errors (e.g. "Invalid credentials").
		return fmt.Errorf("xtream: decode (status %d): %w", resp.StatusCode, err)
	}
	return nil
}

// ParseStreamID converts an external stream id ("1234") to int64.
func ParseStreamID(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}
