package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/thooton/delta/internal/remarkable"
)

// The user's reMarkable tablet, through the rmfakecloud server on this
// machine, which the tablet syncs with. The command center reads and writes
// rmfakecloud's storage for one user directly, as one more sync client.
//
// rmfakecloud keeps each user's files in <data>/users/<user>/sync/, as the
// reMarkable cloud's "sync 1.5" does: every file is a blob named by the
// SHA-256 of its contents. A document is an index blob listing its files
// (<id>.metadata, <id>.content, <id>.pdf, <id>/<page>.rm, ...), one line
// each, "hash:0:name:0:size". The root index lists the documents,
// "hash:80000000:id:files:size", and the file "root" holds its hash. An
// index's hash is the SHA-256 of its entries' hashes (decoded, in name
// order), not of its text. Folders are documents with only metadata.
//
// Changing anything means writing new blobs, then a new root index, then
// pointing "root" at it. ".root.history" logs every root, one 86-byte line
// each, and its length is the generation a syncing tablet must match, so a
// write that raced with the tablet's makes it sync again rather than lose
// either. rmfakecloud guards the root with flock on that file; so does this.
//
// Then the tablet is told to sync: rmfakecloud's web API passes that on, for
// a token signed with its JWT_SECRET_KEY, read from -remarkable-env. Without
// it, the tablet sees changes only the next time it syncs by itself, so why
// it can't be told is logged, and POST /remarkable/sync asks again and says.
//
// Routes:
//
//	GET   /remarkable                 Every document and folder.
//	GET   /remarkable/{id}            A document's files, as a zip (an .rmdoc).
//	GET   /remarkable/{id}/pages/{n}  Page n (from 1) as the tablet shows it, as a
//	                                  one-page PDF; X-Pages says how many there are.
//	POST  /remarkable/documents       Body: a PDF or EPUB. Query: name, parent.
//	POST  /remarkable/folders         {"name", "parent"}.
//	POST  /remarkable/sync            Tell the tablet to sync now, and say if that failed.
//	PATCH /remarkable/{id}            {"name"?, "parent"?}: rename, move, or
//	                                  move to the trash (parent "trash"). POST
//	                                  does the same, for clients without PATCH.
const (
	remarkableHistory = ".root.history"
	remarkableTrash   = "trash"
)

type remarkableStore struct {
	dir       string // the user's sync directory
	user      string
	envFile   string
	maxUpload int64

	mu    sync.Mutex
	cache map[string]remarkableItem // by document index hash
}

func newRemarkableStore(dataDir, user, envFile string, maxUpload int64) *remarkableStore {
	if dataDir == "" || user == "" {
		return nil
	}
	return &remarkableStore{
		dir:       filepath.Join(dataDir, "users", user, "sync"),
		user:      user,
		envFile:   envFile,
		maxUpload: maxUpload,
		cache:     map[string]remarkableItem{},
	}
}

func (s *remarkableStore) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /remarkable", s.list)
	mux.HandleFunc("GET /remarkable/{id}", s.download)
	mux.HandleFunc("GET /remarkable/{id}/pages/{n}", s.page)
	mux.HandleFunc("POST /remarkable/documents", s.upload)
	mux.HandleFunc("POST /remarkable/folders", s.mkdir)
	mux.HandleFunc("POST /remarkable/sync", s.syncNow)
	mux.HandleFunc("PATCH /remarkable/{id}", s.patch)
	// The same, for clients that can't send PATCH (Java's HttpURLConnection,
	// so the phone's Tablet app).
	mux.HandleFunc("POST /remarkable/{id}", s.patch)
}

func registerRemarkableDisabled(mux *http.ServeMux) {
	mux.HandleFunc("/remarkable", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusServiceUnavailable, "the reMarkable isn't set up on the command center")
	})
	mux.HandleFunc("/remarkable/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusServiceUnavailable, "the reMarkable isn't set up on the command center")
	})
}

type remarkableEntry struct {
	Hash, Type, Name string
	Files            int
	Size             int64
}

type remarkableItem struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Type     string    `json:"type"` // folder, pdf, epub or notebook
	Parent   string    `json:"parent"`
	Pages    int       `json:"pages,omitempty"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

func (s *remarkableStore) blobPath(hash string) (string, error) {
	if len(hash) != 64 || strings.Trim(hash, "0123456789abcdef") != "" {
		return "", fmt.Errorf("bad blob name %q", hash)
	}
	return filepath.Join(s.dir, hash), nil
}

func (s *remarkableStore) readBlob(hash string) ([]byte, error) {
	p, err := s.blobPath(hash)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

func parseRemarkableIndex(data []byte) ([]remarkableEntry, error) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, 1<<20)
	if !sc.Scan() {
		return nil, nil
	}
	schema := sc.Text()
	if schema != "3" && schema != "4" {
		return nil, fmt.Errorf("unknown index schema %q", schema)
	}
	if schema == "4" {
		sc.Scan() // summary: 0:.:count:size
	}
	var out []remarkableEntry
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		f := strings.Split(line, ":")
		if len(f) != 5 {
			return nil, fmt.Errorf("bad index line %q", line)
		}
		files, _ := strconv.Atoi(f[3])
		size, _ := strconv.ParseInt(f[4], 10, 64)
		out = append(out, remarkableEntry{Hash: f[0], Type: f[1], Name: f[2], Files: files, Size: size})
	}
	return out, sc.Err()
}

// rootIndex returns the current root's hash and documents.
func (s *remarkableStore) rootIndex() (string, []remarkableEntry, error) {
	raw, err := os.ReadFile(filepath.Join(s.dir, "root"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, nil
	} else if err != nil {
		return "", nil, err
	}
	hash := strings.TrimSpace(string(raw))
	if hash == "" {
		return "", nil, nil
	}
	data, err := s.readBlob(hash)
	if err != nil {
		return "", nil, err
	}
	docs, err := parseRemarkableIndex(data)
	return hash, docs, err
}

func (s *remarkableStore) docFiles(doc remarkableEntry) ([]remarkableEntry, error) {
	data, err := s.readBlob(doc.Hash)
	if err != nil {
		return nil, err
	}
	return parseRemarkableIndex(data)
}

func fileWithSuffix(files []remarkableEntry, suffix string) (remarkableEntry, bool) {
	for _, f := range files {
		if strings.HasSuffix(f.Name, suffix) && !strings.Contains(f.Name, "/") {
			return f, true
		}
	}
	return remarkableEntry{}, false
}

// item describes a document from its metadata and content files.
func (s *remarkableStore) item(doc remarkableEntry) (remarkableItem, error) {
	if it, ok := s.cache[doc.Hash]; ok {
		return it, nil
	}
	files, err := s.docFiles(doc)
	if err != nil {
		return remarkableItem{}, err
	}
	it := remarkableItem{ID: doc.Name, Size: doc.Size}
	if f, ok := fileWithSuffix(files, ".metadata"); ok {
		data, err := s.readBlob(f.Hash)
		if err != nil {
			return it, err
		}
		var m struct {
			Name         string `json:"visibleName"`
			Type         string `json:"type"`
			Parent       string `json:"parent"`
			LastModified string `json:"lastModified"`
			Deleted      bool   `json:"deleted"`
		}
		json.Unmarshal(data, &m)
		it.Name, it.Parent = m.Name, m.Parent
		if ms, err := strconv.ParseInt(m.LastModified, 10, 64); err == nil {
			it.Modified = time.UnixMilli(ms).UTC()
		}
		if m.Type == "CollectionType" {
			it.Type = "folder"
		}
	}
	if it.Type == "" {
		it.Type = "notebook"
		if f, ok := fileWithSuffix(files, ".content"); ok {
			if data, err := s.readBlob(f.Hash); err == nil {
				var c struct {
					FileType  string `json:"fileType"`
					PageCount int    `json:"pageCount"`
				}
				json.Unmarshal(data, &c)
				if c.FileType != "" {
					it.Type = c.FileType
				}
				it.Pages = c.PageCount
			}
		}
	}
	s.cache[doc.Hash] = it
	return it, nil
}

func (s *remarkableStore) items() ([]remarkableItem, error) {
	_, docs, err := s.rootIndex()
	if err != nil {
		return nil, err
	}
	out := []remarkableItem{}
	for _, d := range docs {
		it, err := s.item(d)
		if err != nil {
			log.Printf("remarkable: skipping %s: %v", d.Name, err)
			continue
		}
		out = append(out, it)
	}
	return out, nil
}

func (s *remarkableStore) list(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	items, err := s.items()
	s.mu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reading the reMarkable's files: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *remarkableStore) download(w http.ResponseWriter, r *http.Request) {
	s.writeZip(w, r.PathValue("id"))
}

// writeZip sends document id's files as a zip.
func (s *remarkableStore) writeZip(w http.ResponseWriter, id string) {
	_, docs, err := s.rootIndex()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, d := range docs {
		if d.Name != id {
			continue
		}
		files, err := s.docFiles(d)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		zw := zip.NewWriter(w)
		for _, f := range files {
			p, err := s.blobPath(f.Hash)
			if err != nil {
				continue
			}
			src, err := os.Open(p)
			if err != nil {
				continue // a missing page leaves the rest readable
			}
			dst, err := zw.CreateHeader(&zip.FileHeader{Name: f.Name, Method: zip.Store})
			if err == nil {
				_, err = io.Copy(dst, src)
			}
			src.Close()
			if err != nil {
				return
			}
		}
		zw.Close()
		return
	}
	writeError(w, http.StatusNotFound, "no such document on the reMarkable")
}

// page sends page n of a document as the tablet shows it (handwriting and
// all, drawn as `delta remarkable get` draws it), as a PDF of that page
// alone, so the phone's Tablet app can show one page at a time without
// fetching the whole document. X-Pages says how many pages there are, also
// on a 404 for a page past the end; X-Handwriting-Only is "true" when they
// are the handwriting alone, on pages of its own (an EPUB, which the tablet
// lays out itself, or a PDF that couldn't be read), not the document's.
func (s *remarkableStore) page(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 1 {
		writeError(w, http.StatusBadRequest, "pages are numbered from 1")
		return
	}
	it, err := s.find(r.PathValue("id"))
	if errors.Is(err, errRemarkableMissing) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if it.Type == "folder" {
		writeError(w, http.StatusBadRequest, "that's a folder; it has no pages")
		return
	}
	files, err := s.readFiles(it.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := remarkable.RenderPage(it.ID, files, n)
	w.Header().Set("X-Pages", strconv.Itoa(out.Pages))
	if out.Separate {
		w.Header().Set("X-Handwriting-Only", "true")
	}
	switch {
	case out.Pages == 0:
		writeError(w, http.StatusNotFound, "nothing is written on it, and the tablet lays out its pages itself")
	case out.PDF == nil:
		writeError(w, http.StatusNotFound, fmt.Sprintf("it has %d pages", out.Pages))
	default:
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Length", strconv.Itoa(len(out.PDF)))
		w.Write(out.PDF)
	}
}

// readFiles reads document id's files into memory, by name, as
// remarkable.Render takes them.
func (s *remarkableStore) readFiles(id string) (map[string][]byte, error) {
	_, docs, err := s.rootIndex()
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		if d.Name != id {
			continue
		}
		entries, err := s.docFiles(d)
		if err != nil {
			return nil, err
		}
		files := map[string][]byte{}
		for _, f := range entries {
			if data, err := s.readBlob(f.Hash); err == nil {
				files[f.Name] = data // a missing page leaves the rest readable
			}
		}
		return files, nil
	}
	return nil, errRemarkableMissing
}

// find describes document id.
func (s *remarkableStore) find(id string) (remarkableItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := s.items()
	if err != nil {
		return remarkableItem{}, err
	}
	for _, it := range items {
		if it.ID == id {
			return it, nil
		}
	}
	return remarkableItem{}, errRemarkableMissing
}

var errRemarkableMissing = errors.New("no such document on the reMarkable")

// writeBlob stores data under its hash and returns its entry.
func (s *remarkableStore) writeBlob(name string, data []byte) (remarkableEntry, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	path := filepath.Join(s.dir, hash)
	if _, err := os.Stat(path); err != nil {
		tmp, err := os.CreateTemp(s.dir, ".delta-*")
		if err != nil {
			return remarkableEntry{}, err
		}
		_, err = tmp.Write(data)
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Chmod(tmp.Name(), 0o644)
		}
		if err == nil {
			err = os.Rename(tmp.Name(), path)
		}
		if err != nil {
			os.Remove(tmp.Name())
			return remarkableEntry{}, err
		}
	}
	return remarkableEntry{Hash: hash, Type: "0", Name: name, Size: int64(len(data))}, nil
}

func remarkableHash(entries []remarkableEntry) string {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	h := sha256.New()
	for _, e := range entries {
		b, _ := hex.DecodeString(e.Hash)
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func remarkableIndexText(entries []remarkableEntry) []byte {
	var b bytes.Buffer
	b.WriteString("3\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "%s:%s:%s:%d:%d\n", e.Hash, e.Type, e.Name, e.Files, e.Size)
	}
	return b.Bytes()
}

// writeIndex stores an index blob for entries and returns its hash.
func (s *remarkableStore) writeIndex(entries []remarkableEntry) (string, error) {
	hash := remarkableHash(entries)
	path := filepath.Join(s.dir, hash)
	if _, err := os.Stat(path); err == nil {
		return hash, nil
	}
	tmp, err := os.CreateTemp(s.dir, ".delta-*")
	if err != nil {
		return "", err
	}
	_, err = tmp.Write(remarkableIndexText(entries))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return hash, err
}

// docEntry writes a document's index and returns its root entry.
func (s *remarkableStore) docEntry(id string, files []remarkableEntry) (remarkableEntry, error) {
	hash, err := s.writeIndex(files)
	if err != nil {
		return remarkableEntry{}, err
	}
	var size int64
	for _, f := range files {
		size += f.Size
	}
	return remarkableEntry{Hash: hash, Type: "80000000", Name: id, Files: len(files), Size: size}, nil
}

// update changes the list of documents: under rmfakecloud's lock, it reads
// the current root, lets change edit it, and writes the result as the new
// root. Then it tells the tablet to sync.
func (s *remarkableStore) update(change func(docs []remarkableEntry) ([]remarkableEntry, error)) error {
	history := filepath.Join(s.dir, remarkableHistory)
	hf, err := os.OpenFile(history, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer hf.Close()
	// rmfakecloud gives up at once when the lock is held, and the tablet
	// retries, so this holds it only briefly; it waits for its own turn.
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := syscall.Flock(int(hf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			return fmt.Errorf("can't lock the reMarkable's files: %w", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer syscall.Flock(int(hf.Fd()), syscall.LOCK_UN)

	_, docs, err := s.rootIndex()
	if err != nil {
		return err
	}
	docs, err = change(docs)
	if err != nil {
		return err
	}
	hash, err := s.writeIndex(docs)
	if err != nil {
		return err
	}
	line := time.Now().UTC().Format(time.RFC3339) + " " + hash + "\n"
	if _, err := hf.WriteString(line); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.dir, "root"), []byte(hash), 0o644); err != nil {
		return err
	}
	go func() {
		if err := s.notify(); err != nil {
			log.Printf("remarkable: couldn't tell the tablet to sync: %v", err)
		}
	}()
	return nil
}

// syncNow tells the tablet to sync, and says whether that worked.
func (s *remarkableStore) syncNow(w http.ResponseWriter, r *http.Request) {
	if err := s.notify(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "couldn't tell the tablet to sync: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"synced": true})
}

// notify asks rmfakecloud to tell the tablet to sync now.
func (s *remarkableStore) notify() error {
	env, err := os.ReadFile(s.envFile)
	if err != nil {
		return fmt.Errorf("can't read rmfakecloud's settings: %w", err)
	}
	secret, port := "", "3000"
	for _, line := range strings.Split(string(env), "\n") {
		k, v, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "export "), "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "JWT_SECRET_KEY":
			secret = v
		case "PORT":
			port = v
		}
	}
	if secret == "" {
		return errors.New("rmfakecloud's settings have no JWT_SECRET_KEY")
	}
	token, err := rmfakecloudToken(secret, s.user)
	if err != nil {
		return err
	}
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+port+"/ui/api/sync", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rmfakecloud answered HTTP %d", resp.StatusCode)
	}
	return nil
}

// rmfakecloudToken signs a short-lived token for rmfakecloud's web API, as
// its login does: HS256, keyed with PBKDF2 of JWT_SECRET_KEY.
func rmfakecloudToken(secret, user string) (string, error) {
	key, err := pbkdf2.Key(sha256.New, secret, []byte("todo some salt"), 10000, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "kid": "1", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"UserID":    user,
		"BrowserID": "delta",
		"Email":     user,
		"scopes":    "sync15",
		"Roles":     []string{"User"},
		"iss":       "rmFake WEB",
		"aud":       []string{"web"},
		"exp":       time.Now().Add(5 * time.Minute).Unix(),
	})
	unsigned := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(unsigned))
	return unsigned + "." + enc.EncodeToString(mac.Sum(nil)), nil
}

func newRemarkableID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func remarkableNow() string { return strconv.FormatInt(time.Now().UnixMilli(), 10) }

// newMetadata is a new document's or folder's metadata, as rmfakecloud
// writes it.
func newMetadata(name, kind, parent string) []byte {
	now := remarkableNow()
	data, _ := json.Marshal(map[string]any{
		"visibleName":      name,
		"type":             kind,
		"parent":           parent,
		"createdTime":      now,
		"lastModified":     now,
		"lastOpened":       "",
		"lastOpenedPage":   0,
		"version":          1,
		"pinned":           false,
		"synced":           true,
		"modified":         false,
		"deleted":          false,
		"metadatamodified": true,
	})
	return data
}

func newContent(fileType string) []byte {
	data, _ := json.Marshal(map[string]any{
		"dummyDocument": false,
		"extraMetadata": map[string]string{
			"LastPen": "Finelinerv2", "LastTool": "Finelinerv2", "ThicknessScale": "", "LastFinelinerv2Size": "1",
		},
		"fileType":       fileType,
		"fontName":       "",
		"lastOpenedPage": 0,
		"lineHeight":     -1,
		"margins":        180,
		"orientation":    "portrait",
		"pageCount":      0,
		"pages":          []string{},
		"textScale":      1,
		"transform": map[string]int{
			"m11": 1, "m12": 0, "m13": 0, "m21": 0, "m22": 1, "m23": 0, "m31": 0, "m32": 0, "m33": 1,
		},
	})
	return data
}

// checkParent makes sure parent is the top level ("") or a folder.
func (s *remarkableStore) checkParent(docs []remarkableEntry, parent string) error {
	if parent == "" {
		return nil
	}
	for _, d := range docs {
		if d.Name == parent {
			it, err := s.item(d)
			if err != nil {
				return err
			}
			if it.Type != "folder" {
				return fmt.Errorf("%s is not a folder", it.Name)
			}
			return nil
		}
	}
	return errors.New("no such folder on the reMarkable")
}

func validRemarkableName(name string) bool {
	return strings.TrimSpace(name) != "" && len(name) <= 255 && !strings.ContainsAny(name, "\x00\n\r")
}

func (s *remarkableStore) upload(w http.ResponseWriter, r *http.Request) {
	name, parent := r.URL.Query().Get("name"), r.URL.Query().Get("parent")
	if !validRemarkableName(name) {
		writeError(w, http.StatusBadRequest, "the document needs a name")
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxUpload))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file exceeds %d bytes", s.maxUpload))
		return
	}
	id, err := s.add(name, parent, data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

var errRemarkableKind = errors.New("the reMarkable takes PDFs and EPUBs only")

// add puts a PDF or EPUB on the tablet, in folder parent ("" for the top
// level), and returns its id.
func (s *remarkableStore) add(name, parent string, data []byte) (string, error) {
	var kind string
	switch {
	case bytes.HasPrefix(data, []byte("%PDF-")):
		kind = "pdf"
	case bytes.HasPrefix(data, []byte("PK")) && bytes.Contains(data[:min(len(data), 100)], []byte("application/epub+zip")):
		kind = "epub"
	default:
		return "", errRemarkableKind
	}
	id := newRemarkableID()
	var files []remarkableEntry
	for _, f := range []struct {
		name string
		data []byte
	}{
		{id + ".metadata", newMetadata(name, "DocumentType", parent)},
		{id + ".content", newContent(kind)},
		{id + "." + kind, data},
	} {
		e, err := s.writeBlob(f.name, f.data)
		if err != nil {
			return "", err
		}
		files = append(files, e)
	}
	return id, s.create(id, parent, files)
}

func (s *remarkableStore) mkdir(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name, Parent string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil || !validRemarkableName(req.Name) {
		writeError(w, http.StatusBadRequest, `expected {"name": ..., "parent": ...}`)
		return
	}
	id := newRemarkableID()
	meta, err := s.writeBlob(id+".metadata", newMetadata(req.Name, "CollectionType", req.Parent))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.create(id, req.Parent, []remarkableEntry{meta}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// create adds a document or folder whose files are written.
func (s *remarkableStore) create(id, parent string, files []remarkableEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.docEntry(id, files)
	if err != nil {
		return err
	}
	return s.update(func(docs []remarkableEntry) ([]remarkableEntry, error) {
		if err := s.checkParent(docs, parent); err != nil {
			return nil, err
		}
		return append(docs, doc), nil
	})
}

func (s *remarkableStore) patch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Name   *string `json:"name"`
		Parent *string `json:"parent"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, `expected {"name"?: ..., "parent"?: ...}`)
		return
	}
	if req.Name != nil && !validRemarkableName(*req.Name) {
		writeError(w, http.StatusBadRequest, "the name can't be empty")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	notFound := errors.New("no such document on the reMarkable")
	err := s.update(func(docs []remarkableEntry) ([]remarkableEntry, error) {
		i := -1
		for j, d := range docs {
			if d.Name == id {
				i = j
			}
		}
		if i < 0 {
			return nil, notFound
		}
		if req.Parent != nil && *req.Parent != remarkableTrash {
			if err := s.checkParent(docs, *req.Parent); err != nil {
				return nil, err
			}
			// A folder can't go inside itself.
			parents := map[string]string{}
			for _, d := range docs {
				if it, err := s.item(d); err == nil {
					parents[it.ID] = it.Parent
				}
			}
			for p, n := *req.Parent, 0; p != "" && n < 1000; p, n = parents[p], n+1 {
				if p == id {
					return nil, errors.New("a folder can't be moved inside itself")
				}
			}
		}
		files, err := s.docFiles(docs[i])
		if err != nil {
			return nil, err
		}
		for j, f := range files {
			if f.Name != id+".metadata" {
				continue
			}
			data, err := s.readBlob(f.Hash)
			if err != nil {
				return nil, err
			}
			var m map[string]any
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, fmt.Errorf("unreadable metadata: %w", err)
			}
			if req.Name != nil {
				m["visibleName"] = *req.Name
			}
			if req.Parent != nil {
				m["parent"] = *req.Parent
			}
			m["lastModified"] = remarkableNow()
			m["metadatamodified"] = true
			data, _ = json.Marshal(m)
			if files[j], err = s.writeBlob(f.Name, data); err != nil {
				return nil, err
			}
			doc, err := s.docEntry(id, files)
			if err != nil {
				return nil, err
			}
			docs[i] = doc
			return docs, nil
		}
		return nil, errors.New("the document has no metadata")
	})
	if errors.Is(err, notFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	} else if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}
