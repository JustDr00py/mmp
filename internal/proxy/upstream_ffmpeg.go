package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ffmpegStderrTail is how much of ffmpeg's stderr is kept for the error
// reported when it exits badly.
const ffmpegStderrTail = 2048

// FFmpegOpener runs a candidate through ffmpeg (a stream profile in ffmpeg
// mode). MMP still opens the upstream itself, with Upstream, and pipes the
// TS into ffmpeg's stdin; ffmpeg writes TS to stdout. So provider URLs and
// credentials never reach ffmpeg's argv or its log, and HLS, redirects and
// connect timeouts behave exactly as without a profile.
type FFmpegOpener struct {
	Path     string // ffmpeg binary; "" = "ffmpeg" from PATH
	Upstream Opener
}

// ffmpegArgv builds the command line around the profile's arguments, which
// sit between the fixed input and output.
func ffmpegArgv(profile []string) []string {
	argv := []string{"-hide_banner", "-nostats", "-loglevel", "error",
		// The TS demuxer's default 5s analysis would hold output back long
		// enough to trip the stall watchdog; 1s still finds every stream.
		"-analyzeduration", "1000000", "-probesize", "2000000",
		"-fflags", "+genpts", "-f", "mpegts", "-i", "pipe:0"}
	argv = append(argv, profile...)
	return append(argv, "-f", "mpegts", "pipe:1")
}

// Open opens the upstream, then starts ffmpeg on it. Upstream errors are
// returned as-is so failover classifies them as usual.
func (o *FFmpegOpener) Open(ctx context.Context, c Candidate) (io.ReadCloser, error) {
	profile, err := ParseFFmpegArgs(c.FFmpegArgs)
	if err != nil {
		return nil, fmt.Errorf("ffmpeg profile: %w", err)
	}
	src, err := o.Upstream.Open(ctx, c)
	if err != nil {
		return nil, err
	}
	path := o.Path
	if path == "" {
		path = "ffmpeg"
	}
	sctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(sctx, path, ffmpegArgv(profile)...)
	cmd.WaitDelay = 2 * time.Second // bound pipe draining after a kill
	setPdeathsig(cmd)
	stderr := &tailBuffer{max: ffmpegStderrTail}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err == nil {
		var stdout io.ReadCloser
		if stdout, err = cmd.StdoutPipe(); err == nil {
			if err = cmd.Start(); err == nil {
				s := &ffmpegStream{cmd: cmd, stdout: stdout, src: src, ctx: sctx, cancel: cancel, stderr: stderr, fed: make(chan struct{})}
				go s.feed(stdin)
				return s, nil
			}
		}
	}
	cancel()
	src.Close()
	return nil, fmt.Errorf("start ffmpeg: %w", err)
}

// ffmpegStream is ffmpeg's stdout. Close (or the end of the output) kills
// and reaps the process, so no zombie outlives the stream.
type ffmpegStream struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	src    io.ReadCloser
	ctx    context.Context // cancelled by Close or the session: kills ffmpeg
	cancel context.CancelFunc
	stderr *tailBuffer

	fed     chan struct{} // closed when feed returns
	feedErr error         // upstream read error; valid once fed is closed

	waitOnce sync.Once
	waitErr  error // why the output ended: nil means plain EOF
}

// feed copies upstream bytes into ffmpeg. When the upstream ends, closing
// stdin lets ffmpeg flush and exit, which ends the output.
func (s *ffmpegStream) feed(stdin io.WriteCloser) {
	defer close(s.fed)
	_, err := io.Copy(stdin, s.src)
	stdin.Close()
	if err != nil && !errors.Is(err, io.ErrClosedPipe) {
		s.feedErr = err // a write error just means ffmpeg is gone; Wait says why
	}
}

func (s *ffmpegStream) Read(p []byte) (int, error) {
	n, err := s.stdout.Read(p)
	if err != nil {
		if werr := s.wait(); werr != nil {
			return n, werr
		}
		return n, io.EOF
	}
	return n, nil
}

// wait reaps ffmpeg (after its output ended or it was killed) and stops the
// feeder. It runs once; every caller gets the same result.
func (s *ffmpegStream) wait() error {
	s.waitOnce.Do(func() {
		werr := s.cmd.Wait()
		stopped := s.ctx.Err() != nil // we killed it: not an ffmpeg failure
		s.cancel()
		s.src.Close() // unblock a feeder stuck reading a stalled upstream
		<-s.fed
		switch {
		case stopped:
		case werr != nil:
			s.waitErr = fmt.Errorf("ffmpeg: %v%s", werr, s.stderr.suffix())
		case s.feedErr != nil:
			s.waitErr = fmt.Errorf("ffmpeg input: %w", s.feedErr)
		}
	})
	return s.waitErr
}

// Close kills ffmpeg, closes the upstream and reaps the process.
func (s *ffmpegStream) Close() error {
	s.cancel()
	s.wait()
	return nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if over := len(t.b) - t.max; over > 0 {
		t.b = append(t.b[:0], t.b[over:]...)
	}
	return len(p), nil
}

// suffix renders the tail for an error message: ": <last line(s)>".
func (t *tailBuffer) suffix() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := strings.TrimSpace(string(t.b))
	if s == "" {
		return ""
	}
	return ": " + strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " | ")), " ")
}

// ParseFFmpegArgs splits a profile's ffmpeg_args like a POSIX shell would
// split words (whitespace, 'single' and "double" quotes, backslash escapes)
// without any expansion; nothing is run through a shell. MMP owns the input,
// the format and the output, so only allowlisted output options survive
// validation (see ffmpegOptions).
func ParseFFmpegArgs(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inWord := false
	var quote rune
	escaped := false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\\':
			escaped, inWord = true, true
		case quote == '"':
			if r == '"' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 || escaped {
		return nil, errors.New("unterminated quote or escape")
	}
	if inWord {
		args = append(args, cur.String())
	}
	if err := validateFFmpegArgs(args); err != nil {
		return nil, err
	}
	return args, nil
}

// ffmpegOptions is the allowlist of output options a profile may set,
// mapped to how many following tokens the option consumes as its value.
// Anything absent — notably -i, -f and positional outputs — is refused:
// MMP supplies the input, the MPEG-TS format, and the pipe output.
var ffmpegOptions = map[string]int{
	// Codecs and rate control.
	"-c": 1, "-c:v": 1, "-c:a": 1, "-c:s": 1, "-c:d": 1,
	"-codec": 1, "-codec:v": 1, "-codec:a": 1, "-codec:s": 1, "-codec:d": 1,
	"-b": 1, "-b:v": 1, "-b:a": 1, "-minrate": 1, "-maxrate": 1, "-bufsize": 1,
	"-crf": 1, "-qp": 1, "-qscale": 1, "-q:v": 1, "-q:a": 1,
	"-preset": 1, "-tune": 1, "-profile": 1, "-profile:v": 1, "-level": 1,
	"-x264-params": 1, "-x265-params": 1, "-flags": 1,
	// Frame layout and GOP.
	"-s": 1, "-r": 1, "-fps_mode": 1, "-pix_fmt": 1,
	"-g": 1, "-keyint_min": 1, "-sc_threshold": 1,
	// Filters.
	"-vf": 1, "-af": 1, "-filter:v": 1, "-filter:a": 1,
	// Audio.
	"-ar": 1, "-ac": 1,
	// Stream selection, mapping, metadata, duration cap.
	"-map": 1, "-metadata": 1, "-t": 1,
	"-vn": 0, "-an": 0, "-sn": 0, "-dn": 0,
	// Threading and MPEG-TS muxing.
	"-threads": 1, "-muxrate": 1, "-muxdelay": 1, "-mpegts_flags": 1,
	"-vtag": 1, "-atag": 1,
}

// validateFFmpegArgs enforces the allowlist. A bare token that is not the
// value of a preceding option would be an extra output file (ffmpeg allows
// several), letting a profile write anywhere the MMP user can; that is the
// main thing this stops.
func validateFFmpegArgs(args []string) error {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-i":
			return errors.New("-i is not allowed: MMP supplies the input")
		case "-f":
			return errors.New("-f is not allowed: MMP sets the MPEG-TS format")
		}
		if !strings.HasPrefix(a, "-") {
			return fmt.Errorf("unexpected argument %q: ffmpeg_args may not name output files or URLs (MMP supplies input and output)", a)
		}
		argc, ok := ffmpegOptions[a]
		if !ok {
			return fmt.Errorf("unknown option %q: only common ffmpeg output options are allowed", a)
		}
		if argc == 1 {
			if i+1 >= len(args) {
				return fmt.Errorf("option %q needs a value", a)
			}
			i++ // the value may be anything, dash-prefixed or not
		}
	}
	return nil
}

// ValidateProfile checks a stream profile's mode and ffmpeg arguments.
func ValidateProfile(mode, ffmpegArgs string) error {
	switch mode {
	case "passthrough":
		return nil
	case "ffmpeg":
		args, err := ParseFFmpegArgs(ffmpegArgs)
		if err != nil {
			return errors.New("ffmpeg_args: " + err.Error())
		}
		if len(args) == 0 { // ffmpeg's defaults would re-encode to MPEG-2
			return errors.New(`ffmpeg_args is required in ffmpeg mode (e.g. "-c copy")`)
		}
		return nil
	}
	return errors.New("mode must be passthrough or ffmpeg")
}

// ProfileOpener sends candidates whose channel has an ffmpeg profile
// through FFmpeg, and the rest straight to Direct.
type ProfileOpener struct {
	Direct Opener
	FFmpeg Opener
}

// NewProfileOpener wraps direct with an ffmpeg stage at ffmpegPath.
func NewProfileOpener(direct Opener, ffmpegPath string) *ProfileOpener {
	return &ProfileOpener{Direct: direct, FFmpeg: &FFmpegOpener{Path: ffmpegPath, Upstream: direct}}
}

func (p *ProfileOpener) Open(ctx context.Context, c Candidate) (io.ReadCloser, error) {
	if c.FFmpeg {
		return p.FFmpeg.Open(ctx, c)
	}
	return p.Direct.Open(ctx, c)
}
