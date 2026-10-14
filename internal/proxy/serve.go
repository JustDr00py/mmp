package proxy

import (
	"net/http"
	"time"
)

// WriteTimeout bounds one write to a client: a player that stops reading
// entirely is dropped instead of pinning its handler forever. A merely slow
// one just skips ahead in the ring.
const WriteTimeout = 30 * time.Second

// ServeViewer streams v to w as MPEG-TS until the client leaves (r's
// context) or the session ends for good (every candidate exhausted). It
// writes the response headers itself. The caller still owns v and closes it.
func ServeViewer(w http.ResponseWriter, r *http.Request, v *Viewer) {
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()

	buf := make([]byte, 64*1024)
	for {
		n, err := v.Read(r.Context(), buf)
		if n > 0 {
			_ = rc.SetWriteDeadline(time.Now().Add(WriteTimeout))
			wn, werr := w.Write(buf[:n])
			v.m.cfg.Metrics.addViewerBytes(wn)
			if werr != nil {
				return // client gone
			}
			_ = rc.Flush()
		}
		if err != nil {
			return // client left (ctx) or the session ended (EOF)
		}
	}
}
