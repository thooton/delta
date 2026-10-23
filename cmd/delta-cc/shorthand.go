package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The shorthand reader: Gregg shorthand on a page, read by models that run on
// a computer of the user's (a home PC with a GPU, behind NAT), for transform
// jobs whose documents are written in shorthand.
//
// The reader can't be reached from here, so it reaches out instead: its
// worker long-polls the command center for requests and posts each result
// back. A request waits on the command center until the worker answers, so
// to the caller it is one HTTP call. When no worker has polled lately the
// request is refused at once with "shorthand service offline", and the
// agent reads the shorthand itself.
//
// For one image (a page, a line, or a region of either) the worker returns,
// line by line, the Grascii its OCR model read (the strokes, in ASCII), the
// dictionary's candidate words for each outline, and the English its
// Grascii-to-English model made of the line.
//
// The worker signs in with its own secret, made by `delta-cc
// shorthand-secret` (only its hash is kept, in shorthandSecretFile). It
// reaches the worker routes and nothing else; nothing else reaches them.
//
// Routes, for a transform job's key:
//
//	POST /transform/shorthand  Body: an image (PNG, JPEG, WebP or BMP). Query: mode (page,
//	                           the default, or line: the image is one line), region
//	                           (x,y,w,h in the image's pixels: read only that part).
//	                           Responds with the worker's result (see shorthandResult in
//	                           the worker), 503 when it is offline or busy, 504 when it
//	                           doesn't answer in time.
//
// For the delta token:
//
//	POST /shorthand            The same.
//	GET  /shorthand            {"online", "last_seen", "queued", "reading", "worker"}: whether
//	                           a worker is connected, and what it said about itself.
//	POST /shorthand/reload     {"g2e"?}: the worker reloads its models, switching the
//	                           Grascii-to-English model to that checkpoint (a subfolder of
//	                           its repository, e.g. "state/best") if one is given.
//
// For the worker's secret:
//
//	POST /shorthand/worker/next         Body: {"info"?} about the worker. Waits up to
//	                                    shorthandPoll for a request: 200 and
//	                                    {"id", "kind", "image" (base64), "mode", "region", "g2e"},
//	                                    or 204 when there was none.
//	POST /shorthand/worker/result/{id}  Body: the result as JSON, or {"error", "status"?}
//	                                    (status 400 for an image it can't read).
const (
	shorthandSecretFile  = "shorthand-worker.sha256"
	shorthandSecretStart = "delta_shorthand_"
	shorthandPoll        = 25 * time.Second // how long a worker's poll waits for a request
	shorthandSeen        = 60 * time.Second // a worker that polled this recently is online
	shorthandWait        = 4 * time.Minute  // how long a caller waits for its result
	shorthandWork        = 3 * time.Minute  // how long a worker may take over one request
	maxShorthandImage    = 25 << 20
	maxShorthandResult   = 8 << 20
	maxShorthandQueue    = 16
)

const shorthandOffline = "shorthand service offline: the computer that reads shorthand isn't connected right now"

type shorthandJob struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"` // "read" or "reload"
	Image  []byte `json:"image,omitempty"`
	Mode   string `json:"mode,omitempty"`
	Region []int  `json:"region,omitempty"`
	G2E    string `json:"g2e,omitempty"`

	done    chan shorthandAnswer
	worker  string    // the worker that took it
	taken   time.Time // when
	created time.Time
}

type shorthandAnswer struct {
	status int
	body   []byte
}

type shorthandService struct {
	secretPath string

	mu      sync.Mutex
	queue   []*shorthandJob
	taken   map[string]*shorthandJob
	changed chan struct{} // closed, and replaced, whenever a request is queued
	polling int           // workers waiting in a poll now
	seen    time.Time     // when a worker last polled or answered
	info    json.RawMessage
}

func newShorthandService(dataDir string) *shorthandService {
	return &shorthandService{
		secretPath: filepath.Join(dataDir, shorthandSecretFile),
		taken:      map[string]*shorthandJob{},
		changed:    make(chan struct{}),
	}
}

// register puts the job's and the delta token's routes on mux, behind
// requireAuth.
func (s *shorthandService) register(mux *http.ServeMux, jobs *transformService) {
	mux.HandleFunc("POST /transform/shorthand", jobs.forJob(func(w http.ResponseWriter, r *http.Request, _ *transformJob) { s.read(w, r) }))
	mux.HandleFunc("POST /shorthand", s.read)
	mux.HandleFunc("GET /shorthand", s.status)
	mux.HandleFunc("POST /shorthand/reload", s.reload)
}

// registerWorker puts the worker's routes on mux, which is not behind
// requireAuth: they check the worker's secret themselves.
func (s *shorthandService) registerWorker(mux *http.ServeMux) {
	mux.HandleFunc("POST /shorthand/worker/next", s.workerOnly(s.next))
	mux.HandleFunc("POST /shorthand/worker/result/{id}", s.workerOnly(s.result))
}

// ---------------------------------------------------------------------------
// The worker's secret
// ---------------------------------------------------------------------------

func genShorthandSecret(args []string) {
	fs := flag.NewFlagSet("shorthand-secret", flag.ExitOnError)
	dataDir := fs.String("data", "data", "data directory")
	fs.Parse(args)
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		log.Fatal(err)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		log.Fatal(err)
	}
	secret := shorthandSecretStart + base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(secret))
	path := filepath.Join(*dataDir, shorthandSecretFile)
	if err := writePrivateFile(path, []byte(hex.EncodeToString(sum[:])+"\n")); err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "The shorthand worker's secret hash is in %s; it takes effect at once, and any earlier secret stops working.\n", path)
	fmt.Fprintln(os.Stderr, "Put this secret in the worker's configuration (SHORTHAND_WORKER_SECRET) now; it cannot be shown again:")
	fmt.Println(secret)
}

// workerOnly runs h for requests with the worker's secret. The hash is read
// each time, so a new secret takes effect without a restart.
func (s *shorthandService) workerOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := os.ReadFile(s.secretPath)
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusServiceUnavailable, "no shorthand worker secret is set; run `delta-cc shorthand-secret` on the command center")
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		want, err := hex.DecodeString(strings.TrimSpace(string(raw)))
		token, ok := bearer(r)
		sum := sha256.Sum256([]byte(token))
		if err != nil || len(want) != sha256.Size || !ok || subtle.ConstantTimeCompare(sum[:], want) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid or missing shorthand worker secret")
			return
		}
		h(w, r)
	}
}

// ---------------------------------------------------------------------------
// Callers
// ---------------------------------------------------------------------------

// online says whether a worker is connected: polling now, or lately.
// s.mu must be held.
func (s *shorthandService) online() bool {
	if s.polling > 0 || time.Since(s.seen) < shorthandSeen {
		return true
	}
	for _, j := range s.taken {
		if time.Since(j.taken) < shorthandWork {
			return true // reading something long
		}
	}
	return false
}

// stranded says whether the request id is still queued with no worker
// left to take it.
func (s *shorthandService) stranded(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.online() {
		return false
	}
	for _, j := range s.queue {
		if j.ID == id {
			return true
		}
	}
	return false
}

// parseRegion reads "x,y,w,h" (whole pixels, w and h above 0).
func parseRegion(v string) ([]int, error) {
	if v == "" {
		return nil, nil
	}
	parts := strings.Split(v, ",")
	if len(parts) != 4 {
		return nil, errors.New("region is x,y,w,h in the image's pixels, e.g. 0,400,1200,300")
	}
	out := make([]int, 4)
	for i, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 || n > 1<<16 {
			return nil, fmt.Errorf("region: %q isn't a whole number of pixels", p)
		}
		out[i] = n
	}
	if out[2] == 0 || out[3] == 0 {
		return nil, errors.New("region: its width and height must be above 0")
	}
	return out, nil
}

// isShorthandImage says whether data is an image the worker reads.
func isShorthandImage(data []byte) bool {
	switch http.DetectContentType(data) {
	case "image/png", "image/jpeg", "image/webp", "image/bmp":
		return true
	}
	return false
}

func (s *shorthandService) read(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "page"
	}
	if mode != "page" && mode != "line" {
		writeError(w, http.StatusBadRequest, "mode is page (find the lines) or line (the image is one line)")
		return
	}
	region, err := parseRegion(r.URL.Query().Get("region"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	image, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxShorthandImage))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the image exceeds %d MB; send a page at about 150 dpi", maxShorthandImage>>20))
		return
	}
	if !isShorthandImage(image) {
		writeError(w, http.StatusBadRequest, "the body must be a PNG, JPEG, WebP or BMP image (render a PDF's page to PNG first)")
		return
	}
	s.ask(w, r, &shorthandJob{Kind: "read", Image: image, Mode: mode, Region: region})
}

func (s *shorthandService) reload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		G2E string `json:"g2e"`
	}
	if data, _ := io.ReadAll(io.LimitReader(r.Body, 4<<10)); len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &req); err != nil {
			writeError(w, http.StatusBadRequest, `expected {"g2e"?: "<subfolder>"}`)
			return
		}
	}
	g := strings.Trim(strings.TrimSpace(req.G2E), "/")
	if strings.Contains(g, "..") || strings.ContainsAny(g, "\\\x00\r\n") {
		writeError(w, http.StatusBadRequest, "g2e is a subfolder of the model repository, like state/best")
		return
	}
	s.ask(w, r, &shorthandJob{Kind: "reload", G2E: g})
}

// ask queues job for the worker and answers with its result.
func (s *shorthandService) ask(w http.ResponseWriter, r *http.Request, job *shorthandJob) {
	id := make([]byte, 12)
	rand.Read(id)
	job.ID = hex.EncodeToString(id)
	job.done = make(chan shorthandAnswer, 1)
	job.created = time.Now()
	s.mu.Lock()
	if !s.online() {
		s.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, shorthandOffline)
		return
	}
	if len(s.queue) >= maxShorthandQueue {
		s.mu.Unlock()
		writeError(w, http.StatusServiceUnavailable, "the shorthand reader is busy; try again in a minute")
		return
	}
	s.queue = append(s.queue, job)
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()

	timer := time.NewTimer(shorthandWait)
	defer timer.Stop()
	check := time.NewTicker(5 * time.Second)
	defer check.Stop()
	for {
		select {
		case a := <-job.done:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(a.status)
			w.Write(a.body)
			return
		case <-check.C:
			if s.stranded(job.ID) {
				s.forget(job.ID)
				writeError(w, http.StatusServiceUnavailable, shorthandOffline)
				return
			}
		case <-timer.C:
			s.forget(job.ID)
			writeError(w, http.StatusGatewayTimeout, "the shorthand reader didn't answer in time; try a smaller region, or read it yourself")
			return
		case <-r.Context().Done():
			s.forget(job.ID)
			return
		}
	}
}

// forget drops a request whose caller stopped waiting.
func (s *shorthandService) forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.taken, id)
	for i, j := range s.queue {
		if j.ID == id {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			return
		}
	}
}

func (s *shorthandService) status(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{"online": s.online(), "queued": len(s.queue), "reading": len(s.taken)}
	if !s.seen.IsZero() {
		out["last_seen"] = s.seen.UTC()
	}
	if s.info != nil {
		out["worker"] = s.info
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// The worker
// ---------------------------------------------------------------------------

func (s *shorthandService) next(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Info   json.RawMessage `json:"info"`
		Worker string          `json:"worker"` // an id the worker picks when it starts
	}
	json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req)
	deadline := time.NewTimer(shorthandPoll)
	defer deadline.Stop()
	s.mu.Lock()
	s.seen = time.Now()
	if len(req.Info) > 0 && json.Valid(req.Info) {
		s.info = req.Info
	}
	// A worker reads one request at a time, so one it took and hasn't
	// answered by its next poll is lost (it failed to post the result), as
	// is one taken too long ago (its worker stopped).
	for id, j := range s.taken {
		if req.Worker != "" && j.worker == req.Worker || time.Since(j.taken) > shorthandWork {
			delete(s.taken, id)
			j.done <- shorthandAnswer{http.StatusBadGateway, errorJSON("the shorthand reader lost this request; try again")}
		}
	}
	s.polling++
	var job *shorthandJob
	for expired := false; r.Context().Err() == nil; {
		if len(s.queue) > 0 {
			job = s.queue[0]
			s.queue = s.queue[1:]
			job.worker, job.taken = req.Worker, time.Now()
			s.taken[job.ID] = job
			break
		}
		if expired {
			break
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-changed:
		case <-deadline.C:
			expired = true
		case <-r.Context().Done():
		}
		s.mu.Lock()
	}
	s.polling--
	s.seen = time.Now()
	s.mu.Unlock()
	if job == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *shorthandService) result(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxShorthandResult))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "the result is too big")
		return
	}
	var e struct {
		Error  string `json:"error"`
		Status int    `json:"status"`
	}
	if !json.Valid(data) || json.Unmarshal(data, &e) != nil {
		writeError(w, http.StatusBadRequest, "the result must be a JSON object")
		return
	}
	status := http.StatusOK
	if e.Error != "" {
		status = http.StatusBadGateway
		if e.Status == http.StatusBadRequest {
			status = http.StatusBadRequest
		}
		data = errorJSON(e.Error)
	}
	s.mu.Lock()
	s.seen = time.Now()
	job, ok := s.taken[r.PathValue("id")]
	delete(s.taken, r.PathValue("id"))
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "no such request (its caller stopped waiting)")
		return
	}
	job.done <- shorthandAnswer{status, data}
	writeJSON(w, http.StatusOK, map[string]bool{"delivered": true})
}

func errorJSON(msg string) []byte {
	data, _ := json.Marshal(map[string]string{"error": msg})
	return append(data, '\n')
}
