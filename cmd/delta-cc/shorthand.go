package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
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
// back. A reading takes minutes, longer than the proxy in front of the
// command center (Cloudflare, ~100 s) lets one HTTP request wait, so it is
// asynchronous: the caller submits the image and gets an id at once, then
// polls the id with short requests until the result is there. Results are
// kept shorthandKeep after they come, for whoever submitted them only. When
// no worker has polled lately a reading is refused at once with "shorthand
// service offline", and the agent reads the shorthand itself.
//
// For one image (a page, a line, or a region of either) the worker returns,
// line by line, the Grascii its OCR model read (the strokes, in ASCII), the
// dictionary's candidate words for each outline, the draft English its
// Grascii-to-English model made of the line, and the finished English:
// DeepSeek's post-edit of all three (postedited true), or the draft where
// that failed or is off.
//
// The worker signs in with its own secret, made by `delta-cc
// shorthand-secret` (only its hash is kept, in shorthandSecretFile). It
// reaches the worker routes and nothing else; nothing else reaches them.
//
// Routes, for a transform job's key:
//
//	POST /transform/shorthand       Body: an image (PNG, JPEG, WebP or BMP). Query: mode
//	                                (page, the default, or line: the image is one line),
//	                                region (x,y,w,h in the image's pixels: read only that
//	                                part). 202 and {"id", "status": "pending"} at once; 503
//	                                when the reader is offline or busy.
//	GET  /transform/shorthand/{id}  Always quick. 200 and {"status": "pending", "state":
//	                                "queued" or "reading", "seconds"}, {"status": "done",
//	                                "result"} (see shorthandResult in the worker), or
//	                                {"status": "error", "code", "error"} (code 502 when the
//	                                worker failed or lost it, 503 offline, 504 when it took
//	                                over shorthandWait, 400 for an image it can't read).
//	                                404 for an id this key didn't submit, or one forgotten.
//
// For the delta token:
//
//	POST /shorthand, GET /shorthand/{id}  The same, for readings the token submitted.
//	GET  /shorthand            {"online", "last_seen", "queued", "reading", "worker"}: whether
//	                           a worker is connected, and what it said about itself.
//	POST /shorthand/reload     {"g2e"?}: the worker reloads its models, switching the
//	                           Grascii-to-English model to that checkpoint (a subfolder of
//	                           its repository, e.g. "state/best") if one is given. This one
//	                           waits for the worker's answer (a reload takes seconds).
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
	// The worker's DeepSeek post-edit takes 1-5 minutes a page, and the worker
	// stops it at 9 (POSTEDIT_DEADLINE, 540 s), so a reading can take nearly 10.
	shorthandWait      = 10 * time.Minute // how long a reading may take, queued and read
	shorthandWork      = 10 * time.Minute // how long a worker may take over one request
	shorthandKeep      = 30 * time.Minute // how long a result is kept for its caller to fetch
	maxShorthandImage  = 25 << 20
	maxShorthandResult = 8 << 20
	maxShorthandQueue  = 16
	maxShorthandKept   = 32 // finished readings kept at most (the oldest go first)
)

const shorthandOffline = "shorthand service offline: the computer that reads shorthand isn't connected right now"

type shorthandJob struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"` // "read" or "reload"
	Image  []byte `json:"image,omitempty"`
	Mode   string `json:"mode,omitempty"`
	Region []int  `json:"region,omitempty"`
	G2E    string `json:"g2e,omitempty"`

	done     chan shorthandAnswer // a reload's caller waits on it; nil for a reading
	owner    string               // who submitted a reading, and alone may fetch it
	answer   *shorthandAnswer     // a reading's result, once it came
	finished time.Time            // when
	worker   string               // the worker that took it
	taken    time.Time            // when
	created  time.Time
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
	reads   map[string]*shorthandJob // readings by id, until shorthandKeep after they finish
	changed chan struct{}            // closed, and replaced, whenever a request is queued
	polling int                      // workers waiting in a poll now
	seen    time.Time                // when a worker last polled or answered
	info    json.RawMessage
}

func newShorthandService(dataDir string) *shorthandService {
	return &shorthandService{
		secretPath: filepath.Join(dataDir, shorthandSecretFile),
		taken:      map[string]*shorthandJob{},
		reads:      map[string]*shorthandJob{},
		changed:    make(chan struct{}),
	}
}

// register puts the job's and the delta token's routes on mux, behind
// requireAuth.
func (s *shorthandService) register(mux *http.ServeMux, jobs *transformService) {
	mux.HandleFunc("POST /transform/shorthand", jobs.forJob(func(w http.ResponseWriter, r *http.Request, j *transformJob) { s.read(w, r, "job:"+j.ID) }))
	mux.HandleFunc("GET /transform/shorthand/{id}", jobs.forJob(func(w http.ResponseWriter, r *http.Request, j *transformJob) { s.poll(w, r, "job:"+j.ID) }))
	mux.HandleFunc("POST /shorthand", func(w http.ResponseWriter, r *http.Request) { s.read(w, r, "token") })
	mux.HandleFunc("GET /shorthand/{id}", func(w http.ResponseWriter, r *http.Request) { s.poll(w, r, "token") })
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
	secret := shorthandSecretStart + randomToken(32)
	path, err := writeKeyHash(*dataDir, shorthandSecretFile, secret)
	if err != nil {
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
	return !s.online() && s.queued(id)
}

// queued says whether the request id is waiting for a worker. s.mu must be
// held.
func (s *shorthandService) queued(id string) bool {
	for _, j := range s.queue {
		if j.ID == id {
			return true
		}
	}
	return false
}

// finish ends job with answer a: a reading keeps it for its caller to
// fetch, a reload's caller gets it. s.mu must be held.
func (s *shorthandService) finish(job *shorthandJob, a shorthandAnswer) {
	delete(s.taken, job.ID)
	for i, j := range s.queue {
		if j.ID == job.ID {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			break
		}
	}
	if job.done != nil {
		select {
		case job.done <- a:
		default:
		}
		return
	}
	if job.answer == nil {
		job.answer, job.finished, job.Image = &a, time.Now(), nil
	}
}

// expire ends readings that went stale (queued with no worker, or taken or
// waiting too long) and forgets results kept long enough. s.mu must be held.
func (s *shorthandService) expire() {
	online := s.online()
	var kept []*shorthandJob
	for id, j := range s.reads {
		switch {
		case j.answer != nil && time.Since(j.finished) > shorthandKeep:
			delete(s.reads, id)
			continue
		case j.answer != nil:
		case !online && s.queued(id):
			s.finish(j, shorthandAnswer{http.StatusServiceUnavailable, errorJSON(shorthandOffline)})
		case time.Since(j.created) > shorthandWait:
			s.finish(j, shorthandAnswer{http.StatusGatewayTimeout, errorJSON("the shorthand reader didn't answer in time; try a smaller region, or read it yourself")})
		case !j.taken.IsZero() && time.Since(j.taken) > shorthandWork:
			s.finish(j, shorthandAnswer{http.StatusBadGateway, errorJSON("the shorthand reader lost this request; try again")})
		}
		if j.answer != nil {
			kept = append(kept, j)
		}
	}
	for len(kept) > maxShorthandKept { // drop the oldest results
		old := 0
		for i, j := range kept {
			if j.finished.Before(kept[old].finished) {
				old = i
			}
		}
		delete(s.reads, kept[old].ID)
		kept = append(kept[:old], kept[old+1:]...)
	}
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

func (s *shorthandService) read(w http.ResponseWriter, r *http.Request, owner string) {
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
	job := &shorthandJob{Kind: "read", Image: image, Mode: mode, Region: region, owner: owner}
	if !s.enqueue(w, job) {
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"id": job.ID, "status": "pending"})
}

// poll answers with the state of a reading owner submitted: pending, or its
// result or error.
func (s *shorthandService) poll(w http.ResponseWriter, r *http.Request, owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	j, ok := s.reads[r.PathValue("id")]
	if !ok || j.owner != owner {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no such shorthand reading (results are kept %d minutes after they come)", int(shorthandKeep/time.Minute)))
		return
	}
	if j.answer == nil {
		state := "reading"
		if j.taken.IsZero() {
			state = "queued"
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending", "state": state, "seconds": int(time.Since(j.created).Seconds())})
		return
	}
	if j.answer.status == http.StatusOK {
		writeJSON(w, http.StatusOK, map[string]any{"status": "done", "result": json.RawMessage(j.answer.body)})
		return
	}
	var e struct {
		Error string `json:"error"`
	}
	json.Unmarshal(j.answer.body, &e)
	writeJSON(w, http.StatusOK, map[string]any{"status": "error", "code": j.answer.status, "error": e.Error})
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

// enqueue gives job an id and queues it for the worker; a reading is kept
// in s.reads too. It answers w itself, and returns false, when it can't.
func (s *shorthandService) enqueue(w http.ResponseWriter, job *shorthandJob) bool {
	job.ID = randomHex(12)
	job.created = time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	if !s.online() {
		writeError(w, http.StatusServiceUnavailable, shorthandOffline)
		return false
	}
	if len(s.queue) >= maxShorthandQueue {
		writeError(w, http.StatusServiceUnavailable, "the shorthand reader is busy; try again in a minute")
		return false
	}
	s.queue = append(s.queue, job)
	if job.done == nil {
		s.reads[job.ID] = job
	}
	close(s.changed)
	s.changed = make(chan struct{})
	return true
}

// ask queues job (a reload) for the worker and answers with its result.
func (s *shorthandService) ask(w http.ResponseWriter, r *http.Request, job *shorthandJob) {
	job.done = make(chan shorthandAnswer, 1)
	if !s.enqueue(w, job) {
		return
	}

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

// forget drops a reload whose caller stopped waiting.
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
	for _, j := range s.taken {
		if req.Worker != "" && j.worker == req.Worker || time.Since(j.taken) > shorthandWork {
			s.finish(j, shorthandAnswer{http.StatusBadGateway, errorJSON("the shorthand reader lost this request; try again")})
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
	var send shorthandJob
	if job != nil {
		send = *job // a copy: finish may drop the job's image once it's answered
	}
	s.mu.Unlock()
	if job == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, &send)
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
	if ok {
		s.finish(job, shorthandAnswer{status, data})
	}
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "no such request (it timed out, or its caller stopped waiting)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"delivered": true})
}

func errorJSON(msg string) []byte {
	data, _ := json.Marshal(map[string]string{"error": msg})
	return append(data, '\n')
}
