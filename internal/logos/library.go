// Package logos is MMP's channel logo library: a copy of a community logo
// collection (github.com/tv-logo/tv-logos by default), downloaded and kept
// up to date by MMP, plus logos the admin uploads. MMP serves both at
// /logos/ (players load channel logos from there), and the admin UI
// searches them and suggests a logo for each channel from its name.
//
// On disk, under Dir:
//
//	library/   the downloaded collection (replaced whole on each update)
//	uploads/   the admin's own logos
//	.library.json  when the library was last downloaded
package logos

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// URLPrefix is where logos are served.
	URLPrefix = "/logos/"
	// DefaultArchiveURL is the tv-logos collection as a tarball of its main
	// branch.
	DefaultArchiveURL = "https://codeload.github.com/tv-logo/tv-logos/tar.gz/refs/heads/main"

	libraryDir = "library"
	uploadsDir = "uploads"
	stateFile  = ".library.json"

	// MaxUpload bounds one uploaded logo.
	MaxUpload = 5 << 20
)

// imageTypes are the files the library holds, by extension.
var imageTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".svg": "image/svg+xml",
}

// Entry is one logo file.
type Entry struct {
	Path     string // under Dir, slash-separated: "library/countries/…/cw-5-ktla-us.png"
	Name     string // file name
	Folder   string // where it sits, for display: "countries/united-states/us-local/cw"
	Uploaded bool

	words   []string // file name words, country code and extension removed
	joined  string   // the words run together: "trutv"
	folders []string // folder words
	country string   // country code the file name ends with ("us"), if any
}

// State is the downloaded library's status.
type State struct {
	Updated time.Time `json:"updated"`
	Files   int       `json:"files"`
	Source  string    `json:"source"`
	// Running, Error and Tried describe the latest update attempt.
	Running bool      `json:"-"`
	Error   string    `json:"-"`
	Tried   time.Time `json:"-"`
}

// Library is the logo collection. Its methods are safe for concurrent use.
type Library struct {
	Dir        string
	ArchiveURL string
	// BaseURL is MMP's public URL (no trailing slash): logo URLs are built
	// from it.
	BaseURL string
	Client  *http.Client
	Log     *slog.Logger

	mu        sync.RWMutex
	entries   []*Entry
	byWord    map[string][]*Entry
	byJoined  map[string][]*Entry // by Entry.joined
	byPath    map[string]*Entry
	countries map[string]bool // country codes file names end with
	stations  map[string]bool // call letters of the local stations (us-local/…)
	state     State
	updating  sync.Mutex
}

// Open prepares dir (creating it and uploads/) and indexes what is there.
func Open(dir, archiveURL, baseURL string, client *http.Client, log *slog.Logger) (*Library, error) {
	if err := os.MkdirAll(filepath.Join(dir, uploadsDir), 0o755); err != nil {
		return nil, fmt.Errorf("logos: %w", err)
	}
	l := &Library{Dir: dir, ArchiveURL: archiveURL, BaseURL: strings.TrimSuffix(baseURL, "/"), Client: client, Log: log}
	if l.ArchiveURL == "" {
		l.ArchiveURL = DefaultArchiveURL
	}
	if l.Client == nil {
		l.Client = http.DefaultClient
	}
	if l.Log == nil {
		l.Log = slog.New(slog.DiscardHandler)
	}
	// Leftovers of an update cut short by a restart.
	if old, _ := filepath.Glob(filepath.Join(dir, ".library-*")); len(old) > 0 {
		for _, p := range old {
			os.RemoveAll(p)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, stateFile)); err == nil {
		_ = json.Unmarshal(b, &l.state)
	}
	if err := l.Reindex(); err != nil {
		return nil, err
	}
	return l, nil
}

// Reindex re-reads the files on disk.
func (l *Library) Reindex() error {
	var entries []*Entry
	for _, top := range []string{libraryDir, uploadsDir} {
		root := filepath.Join(l.Dir, top)
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if strings.HasPrefix(d.Name(), ".") && p != root {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() || !d.Type().IsRegular() || imageTypes[strings.ToLower(filepath.Ext(p))] == "" {
				return nil
			}
			rel, err := filepath.Rel(l.Dir, p)
			if err != nil {
				return err
			}
			entries = append(entries, newEntry(filepath.ToSlash(rel)))
			return nil
		})
		if err != nil {
			return fmt.Errorf("logos: index %s: %w", top, err)
		}
	}

	// A folder's country code is the word most of its file names end
	// with: countries/united-states/… files end in "-us".
	ends := map[string]map[string]int{}
	for _, e := range entries {
		if c := countryFolder(e.Path); c != "" && len(e.words) > 1 {
			last := e.words[len(e.words)-1]
			if ends[c] == nil {
				ends[c] = map[string]int{}
			}
			ends[c][last]++
		}
	}
	codes := map[string]string{} // folder -> code
	for folder, counts := range ends {
		best, n, total := "", 0, 0
		for w, c := range counts {
			total += c
			if c > n || (c == n && w < best) {
				best, n = w, c
			}
		}
		if len(best) >= 2 && len(best) <= 3 && n*2 > total {
			codes[folder] = best
		}
	}
	countries := map[string]bool{}
	byWord, byJoined, byPath := map[string][]*Entry{}, map[string][]*Entry{}, map[string]*Entry{}
	for _, e := range entries {
		if code := codes[countryFolder(e.Path)]; code != "" && len(e.words) > 1 && e.words[len(e.words)-1] == code {
			e.country = code
			e.words = e.words[:len(e.words)-1]
			countries[code] = true
		}
		for _, w := range e.words {
			if l := byWord[w]; len(l) == 0 || l[len(l)-1] != e {
				byWord[w] = append(byWord[w], e)
			}
		}
		e.joined = strings.Join(e.words, "")
		byJoined[e.joined] = append(byJoined[e.joined], e)
		byPath[e.Path] = e
	}
	// Call letters found only on local stations' logos.
	stations := map[string]bool{}
	for w, es := range byWord {
		if isCallLetters(w) && !slices.ContainsFunc(es, func(e *Entry) bool { return !strings.Contains(e.Path, "/us-local/") }) {
			stations[w] = true
		}
	}
	l.mu.Lock()
	l.stations = stations
	l.entries, l.byWord, l.byJoined, l.byPath, l.countries = entries, byWord, byJoined, byPath, countries
	l.mu.Unlock()
	return nil
}

// countryFolder is "united-states" for library/countries/united-states/…
func countryFolder(p string) string {
	parts := strings.Split(p, "/")
	if len(parts) > 3 && parts[0] == libraryDir && parts[1] == "countries" {
		return parts[2]
	}
	return ""
}

func newEntry(p string) *Entry {
	dir, name := path.Split(p)
	dir = strings.TrimSuffix(dir, "/")
	e := &Entry{Path: p, Name: name, Uploaded: strings.HasPrefix(p, uploadsDir+"/")}
	e.Folder = strings.TrimPrefix(strings.TrimPrefix(dir, libraryDir), "/")
	if e.Uploaded {
		e.Folder = "uploads"
	}
	e.words = words(strings.TrimSuffix(name, path.Ext(name)))
	e.folders = words(e.Folder)
	return e
}

// URL is the public URL of the logo at p (an Entry.Path).
func (l *Library) URL(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return l.BaseURL + URLPrefix + strings.Join(parts, "/")
}

// PathOf is the Entry.Path a logo URL points at, if it is one of ours.
func (l *Library) PathOf(u string) (string, bool) {
	rest, ok := strings.CutPrefix(u, l.BaseURL+URLPrefix)
	if !ok {
		return "", false
	}
	p, err := url.PathUnescape(rest)
	return p, err == nil
}

// Lookup returns the entry at p.
func (l *Library) Lookup(p string) (*Entry, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	e, ok := l.byPath[p]
	return e, ok
}

// Counts are the library's and the uploads' file counts.
func (l *Library) Counts() (library, uploads int) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, e := range l.entries {
		if e.Uploaded {
			uploads++
		} else {
			library++
		}
	}
	return library, uploads
}

// Uploads lists the uploaded logos by name.
func (l *Library) Uploads() []Hit {
	l.mu.RLock()
	var hits []Hit
	for _, e := range l.entries {
		if e.Uploaded {
			hits = append(hits, Hit{Entry: e})
		}
	}
	l.mu.RUnlock()
	slices.SortFunc(hits, func(a, b Hit) int { return strings.Compare(a.Name, b.Name) })
	for i := range hits {
		hits[i].URL = l.URL(hits[i].Path)
	}
	return hits
}

// State reports the downloaded library's status.
func (l *Library) State() State {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.state
}

// ErrUpload is an uploaded file MMP will not keep; its message is for the
// admin.
type ErrUpload string

func (e ErrUpload) Error() string { return string(e) }

// Upload saves an uploaded logo as uploads/<slug of name>.<ext>, adding
// -2, -3… if the name is taken, and returns its entry. The type comes
// from the content, not the file name.
func (l *Library) Upload(name string, data []byte) (*Entry, error) {
	if len(data) == 0 {
		return nil, ErrUpload("The file is empty.")
	}
	if len(data) > MaxUpload {
		return nil, ErrUpload("Logos can be at most 5 MB.")
	}
	ext := imageExt(data)
	if ext == "" {
		return nil, ErrUpload("That is not a PNG, JPEG, GIF, WebP or SVG image.")
	}
	stem := slug(strings.TrimSuffix(name, filepath.Ext(name)))
	if stem == "" {
		stem = "logo"
	}
	dir := filepath.Join(l.Dir, uploadsDir)
	f, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return nil, err
	}
	for i := 1; ; i++ {
		file := stem + ext
		if i > 1 {
			file = stem + "-" + strconv.Itoa(i) + ext
		}
		// Link, not rename: it fails instead of replacing a file that
		// appeared since the check.
		err := os.Link(tmp, filepath.Join(dir, file))
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := l.Reindex(); err != nil {
			return nil, err
		}
		e, _ := l.Lookup(uploadsDir + "/" + file)
		if e == nil {
			return nil, fmt.Errorf("logos: %s missing after upload", file)
		}
		return e, nil
	}
}

// DeleteUpload removes an uploaded logo by file name.
func (l *Library) DeleteUpload(name string) error {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return fs.ErrNotExist
	}
	if err := os.Remove(filepath.Join(l.Dir, uploadsDir, name)); err != nil {
		return err
	}
	return l.Reindex()
}

// imageExt is the extension for an image's content, or "" for anything
// else.
func imageExt(data []byte) string {
	switch http.DetectContentType(data) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	head := bytes.ToLower(data[:min(len(data), 4096)])
	if bytes.Contains(head, []byte("<svg")) {
		return ".svg"
	}
	return ""
}

// slug turns a name into a file name stem: "CW 5 (KTLA)" -> "cw-5-ktla".
func slug(name string) string {
	s := strings.Join(words(name), "-")
	if len(s) > 80 {
		s = strings.TrimRight(s[:80], "-")
	}
	return s
}
