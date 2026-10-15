package xtream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// FlexString decodes a JSON string, number, boolean or null as a string:
// panels disagree on (and sometimes mix) the types of ids, ratings and
// timestamps.
type FlexString string

// UnmarshalJSON accepts any scalar.
func (f *FlexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case len(b) == 0 || string(b) == "null":
		*f = ""
	case b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = FlexString(s)
	case b[0] == '[' || b[0] == '{':
		*f = "" // a structure where a scalar was expected: treat as unknown
	default:
		*f = FlexString(b)
	}
	return nil
}

// Int parses the value as an integer (0 if empty or invalid).
func (f FlexString) Int() int64 {
	s := strings.TrimSpace(string(f))
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if x, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(x)
	}
	return 0
}

// Float parses the value as a number; ok is false if empty or invalid.
func (f FlexString) Float() (float64, bool) {
	x, err := strconv.ParseFloat(strings.TrimSpace(string(f)), 64)
	return x, err == nil
}

// VODCategory is one movie or series category.
type VODCategory struct {
	CategoryID   FlexString `json:"category_id"`
	CategoryName string     `json:"category_name"`
}

// VODStream is one movie from get_vod_streams.
type VODStream struct {
	Name               string     `json:"name"`
	StreamID           FlexString `json:"stream_id"`
	StreamIcon         string     `json:"stream_icon"`
	Rating             FlexString `json:"rating"`
	Added              FlexString `json:"added"`
	CategoryID         FlexString `json:"category_id"`
	ContainerExtension string     `json:"container_extension"`
	TMDB               FlexString `json:"tmdb"`
}

// Series is one show from get_series.
type Series struct {
	Name         string     `json:"name"`
	SeriesID     FlexString `json:"series_id"`
	Cover        string     `json:"cover"`
	Plot         string     `json:"plot"`
	Cast         string     `json:"cast"`
	Director     string     `json:"director"`
	Genre        string     `json:"genre"`
	ReleaseDate  FlexString `json:"releaseDate"`
	ReleaseDate2 FlexString `json:"release_date"` // some panels use snake case
	LastModified FlexString `json:"last_modified"`
	Rating       FlexString `json:"rating"`
	CategoryID   FlexString `json:"category_id"`
}

// Released is the release date whichever key the panel used.
func (s Series) Released() string {
	if s.ReleaseDate != "" {
		return string(s.ReleaseDate)
	}
	return string(s.ReleaseDate2)
}

// Episode is one episode from get_series_info.
type Episode struct {
	ID                 FlexString      `json:"id"`
	EpisodeNum         FlexString      `json:"episode_num"`
	Title              string          `json:"title"`
	ContainerExtension string          `json:"container_extension"`
	Season             FlexString      `json:"season"`
	Added              FlexString      `json:"added"`
	Info               json.RawMessage `json:"info"` // an object, or [] when empty
}

// EpisodeDetails is the useful part of an episode's info block.
type EpisodeDetails struct {
	Plot         string     `json:"plot"`
	DurationSecs FlexString `json:"duration_secs"`
	MovieImage   string     `json:"movie_image"`
}

// Details decodes the episode's info block; an empty or odd block yields
// zero values.
func (e Episode) Details() EpisodeDetails {
	var d EpisodeDetails
	if len(e.Info) > 0 && e.Info[0] == '{' {
		_ = json.Unmarshal(e.Info, &d)
	}
	return d
}

// SeriesInfo is get_series_info: the show's info block (kept raw for
// re-serving) and its episodes.
type SeriesInfo struct {
	Info     json.RawMessage
	Episodes []Episode
}

func (si *SeriesInfo) UnmarshalJSON(b []byte) error {
	var raw struct {
		Info     json.RawMessage `json:"info"`
		Episodes json.RawMessage `json:"episodes"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	si.Info = raw.Info
	eps := bytes.TrimSpace(raw.Episodes)
	switch {
	case len(eps) == 0 || string(eps) == "null":
	case eps[0] == '{': // {"1": [...], "2": [...]} keyed by season
		var bySeason map[string][]Episode
		if err := json.Unmarshal(eps, &bySeason); err != nil {
			return fmt.Errorf("episodes: %w", err)
		}
		for season, list := range bySeason {
			for _, e := range list {
				if e.Season == "" {
					e.Season = FlexString(season)
				}
				si.Episodes = append(si.Episodes, e)
			}
		}
	case eps[0] == '[': // [[...], [...]] by season, or a flat list
		var nested [][]Episode
		if err := json.Unmarshal(eps, &nested); err == nil {
			for i, list := range nested {
				for _, e := range list {
					if e.Season == "" {
						e.Season = FlexString(strconv.Itoa(i + 1))
					}
					si.Episodes = append(si.Episodes, e)
				}
			}
			return nil
		}
		if err := json.Unmarshal(eps, &si.Episodes); err != nil {
			return fmt.Errorf("episodes: %w", err)
		}
	}
	return nil
}

// VODCategories fetches the movie categories.
func (c *Client) VODCategories(ctx context.Context) ([]VODCategory, error) {
	var out []VODCategory
	return out, c.getList(ctx, "get_vod_categories", nil, &out)
}

// VODStreams fetches every movie.
func (c *Client) VODStreams(ctx context.Context) ([]VODStream, error) {
	var out []VODStream
	return out, c.getList(ctx, "get_vod_streams", nil, &out)
}

// SeriesCategories fetches the series categories.
func (c *Client) SeriesCategories(ctx context.Context) ([]VODCategory, error) {
	var out []VODCategory
	return out, c.getList(ctx, "get_series_categories", nil, &out)
}

// Series fetches every show (without episodes).
func (c *Client) Series(ctx context.Context) ([]Series, error) {
	var out []Series
	return out, c.getList(ctx, "get_series", nil, &out)
}

// SeriesInfo fetches one show's info and episodes.
func (c *Client) SeriesInfo(ctx context.Context, seriesID string) (*SeriesInfo, error) {
	var out SeriesInfo
	if err := c.get(ctx, c.apiURLWith("get_series_info", url.Values{"series_id": {seriesID}}), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MovieURL is a movie's playback URL on the panel (credentials inside:
// redact before logging).
func (c *Client) MovieURL(streamID, ext string) string {
	return fmt.Sprintf("%s/movie/%s/%s/%s.%s", c.base.String(),
		url.PathEscape(c.username), url.PathEscape(c.password), url.PathEscape(streamID), url.PathEscape(ext))
}

// EpisodeURL is an episode's playback URL on the panel (credentials inside).
func (c *Client) EpisodeURL(episodeID, ext string) string {
	return fmt.Sprintf("%s/series/%s/%s/%s.%s", c.base.String(),
		url.PathEscape(c.username), url.PathEscape(c.password), url.PathEscape(episodeID), url.PathEscape(ext))
}

// getList fetches a list action. Panels answer an empty catalog with [],
// {} or null; all mean "nothing".
func (c *Client) getList(ctx context.Context, action string, extra url.Values, out any) error {
	var raw json.RawMessage
	if err := c.get(ctx, c.apiURLWith(action, extra), &raw); err != nil {
		return err
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("xtream: decode %s: %w", action, err)
	}
	return nil
}

func (c *Client) apiURLWith(action string, extra url.Values) string {
	u := c.apiURL(action)
	if len(extra) > 0 {
		u += "&" + extra.Encode()
	}
	return u
}

// VODInfo fetches get_vod_info for one movie and returns its "info" block
// (an object; nil when the panel has none).
func (c *Client) VODInfo(ctx context.Context, streamID string) (json.RawMessage, error) {
	var out struct {
		Info json.RawMessage `json:"info"`
	}
	if err := c.get(ctx, c.apiURLWith("get_vod_info", url.Values{"vod_id": {streamID}}), &out); err != nil {
		return nil, err
	}
	if b := bytes.TrimSpace(out.Info); len(b) > 0 && b[0] == '{' {
		return out.Info, nil
	}
	return nil, nil
}
