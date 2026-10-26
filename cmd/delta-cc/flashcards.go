package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Flashcards, kept here the way AnkiWeb keeps Anki's collection, so Flashcards
// on the phone and Flashcards on the tablet study the same decks. Each device
// keeps a full copy and works offline; this is only where they meet.
//
// The collection is decks (a name and the new-cards-per-day setting) and
// cards (the text and the FSRS memory state). Every record carries mod, the
// device's clock in milliseconds when it last changed, and usn, the
// collection's sequence number when it last changed here. A sync sends what
// changed on the device since it last synced, and gets back everything whose
// usn is past the one it last saw. Where both sides changed a record, the
// later mod wins, so of two reviews of one card made offline on two devices,
// the more recent counts. A device that sends a record unknown here adds it:
// that is how the phone's first sync brings its decks.
//
// The tablet can't hold the delta token (there is nothing to type it with),
// so it pairs: it asks for a code, shows it, and the user enters it on the
// website, which gives the tablet a token that reaches /flashcards/sync and
// /flashcards and nothing else.
//
// Routes:
//
//	GET    /flashcards                 The whole collection, {"usn", "decks", "cards"}.
//	POST   /flashcards/sync            {"usn", "zone"?, "decks", "cards"}: the device's changes since
//	                                   it last saw usn. Answers {"usn", "decks", "cards"}:
//	                                   every record changed past that usn, and any sent one
//	                                   that lost to a later change.
//	POST   /flashcards/pair            No token. {"name"?}: starts a pairing, {"code", "secret"}.
//	GET    /flashcards/pair/{secret}   No token. 202 while waiting, then {"token"} once.
//	POST   /flashcards/pair/approve    {"code", "name"?}: the delta token approves a pairing
//	                                   (named as given, else as the device named itself).
//	GET    /flashcards/devices         The paired devices, [{"id", "name", "added", "seen"}].
//	DELETE /flashcards/devices/{id}    Unpairs one; its token stops working.
const (
	flashcardsFile        = "flashcards.json"
	flashcardDevicesFile  = "flashcards-devices.json"
	maxFlashcardsBody     = 32 << 20
	maxFlashcardText      = 64 << 10
	maxFlashcardID        = 512
	maxFlashcardCards     = 200000
	maxFlashcardDecks     = 1000
	flashcardTokenStart   = "fc_"
	flashcardPairLifetime = 15 * time.Minute
	maxFlashcardPairings  = 20
	// Leaves out 0/O, 1/I/L and the like, which read alike on e-ink.
	flashcardCodeAlphabet = "ACDEFHJKMNPQRTUVWXY34679"
)

// flashDeck is a deck. NewPerDay is -1 for AUTO: new cards come in while
// the deck's studied cards are recalled well enough, with no daily number.
type flashDeck struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	NewPerDay int    `json:"new_per_day"`
	Mod       int64  `json:"mod"`
	USN       int64  `json:"usn"`
}

// flashCard is one card: its text, and its FSRS state as java-fsrs keeps it
// (state 1 learning, 2 review, 3 relearning; a card never answered has no
// last review). Due, Last and Intro are Unix seconds, 0 for none; Intro is
// when the card was first answered, which a deck's daily new-card limit
// counts.
type flashCard struct {
	ID         string   `json:"id"`
	Deck       string   `json:"deck"`
	Ord        int      `json:"ord"`
	Front      string   `json:"front"`
	Back       string   `json:"back"`
	Aux        string   `json:"aux"`
	State      int      `json:"state"`
	Step       *int     `json:"step"`
	Stability  *float64 `json:"stability"`
	Difficulty *float64 `json:"difficulty"`
	Due        int64    `json:"due"`
	Last       int64    `json:"last"`
	Intro      int64    `json:"intro"`
	Mod        int64    `json:"mod"`
	USN        int64    `json:"usn"`
}

type flashCollection struct {
	USN int64 `json:"usn"`
	// The phone's time zone, which it sends: the day rolls over at 4am
	// there, and the tablet, which keeps UTC, takes it from here.
	Zone  string       `json:"zone,omitempty"`
	Decks []*flashDeck `json:"decks"`
	Cards []*flashCard `json:"cards"`
}

type flashDevice struct {
	ID    string    `json:"id"`
	Name  string    `json:"name"`
	Hash  string    `json:"hash,omitempty"`
	Added time.Time `json:"added"`
	Seen  time.Time `json:"seen"`
}

type flashPairing struct {
	name    string // what the device calls itself
	code    string
	secret  string
	expires time.Time
	token   string // set once approved, until the device fetches it
}

type flashcardService struct {
	path, devicesPath string

	mu         sync.Mutex
	loaded     bool
	collection flashCollection
	decks      map[string]*flashDeck
	cards      map[string]*flashCard

	devMu    sync.Mutex
	pairings []*flashPairing
}

func newFlashcardService(dataDir string) *flashcardService {
	return &flashcardService{
		path:        filepath.Join(dataDir, flashcardsFile),
		devicesPath: filepath.Join(dataDir, flashcardDevicesFile),
	}
}

func (f *flashcardService) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /flashcards", f.get)
	mux.HandleFunc("POST /flashcards/sync", f.sync)
	mux.HandleFunc("POST /flashcards/pair/approve", f.approve)
	mux.HandleFunc("GET /flashcards/devices", f.listDevices)
	mux.HandleFunc("DELETE /flashcards/devices/{id}", f.removeDevice)
}

// registerOpen adds the routes a device uses before it has a token.
func (f *flashcardService) registerOpen(mux *http.ServeMux) {
	mux.HandleFunc("POST /flashcards/pair", f.startPairing)
	mux.HandleFunc("GET /flashcards/pair/{secret}", f.pollPairing)
}

// flashcardsAllowed says whether a paired device's token may make request r.
func flashcardsAllowed(r *http.Request) bool {
	return (r.Method == http.MethodGet && r.URL.Path == "/flashcards") ||
		(r.Method == http.MethodPost && r.URL.Path == "/flashcards/sync") ||
		(r.Method == http.MethodGet && r.URL.Path == "/auth")
}

func (f *flashcardService) get(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.load(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, f.collection)
}

func (f *flashcardService) sync(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxFlashcardsBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a sync exceeds %d bytes", maxFlashcardsBody))
		return
	}
	var given flashCollection
	if err := json.Unmarshal(data, &given); err != nil {
		writeError(w, http.StatusBadRequest, `expected {"usn", "decks", "cards"}`)
		return
	}
	for _, d := range given.Decks {
		if why := checkFlashDeck(d); why != "" {
			writeError(w, http.StatusBadRequest, why)
			return
		}
	}
	for _, c := range given.Cards {
		if why := checkFlashCard(c); why != "" {
			writeError(w, http.StatusBadRequest, why)
			return
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.load(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	since := given.USN
	if since > f.collection.USN {
		// The device saw a collection this one never was (it was reset):
		// it gets everything.
		since = 0
	}
	usn := f.collection.USN + 1
	changed := false
	var lostDecks []*flashDeck
	var lostCards []*flashCard
	for _, d := range given.Decks {
		have := f.decks[d.ID]
		if have != nil && d.Mod <= have.Mod {
			lostDecks = append(lostDecks, have)
			continue
		}
		if have == nil && len(f.decks) >= maxFlashcardDecks {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d decks are kept", maxFlashcardDecks))
			return
		}
		d.USN = usn
		if have == nil {
			f.collection.Decks = append(f.collection.Decks, d)
		} else {
			*have = *d
			d = have
		}
		f.decks[d.ID] = d
		changed = true
	}
	for _, c := range given.Cards {
		have := f.cards[c.ID]
		if have != nil && c.Mod <= have.Mod {
			lostCards = append(lostCards, have)
			continue
		}
		if have == nil && len(f.cards) >= maxFlashcardCards {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d cards are kept", maxFlashcardCards))
			return
		}
		c.USN = usn
		if have == nil {
			f.collection.Cards = append(f.collection.Cards, c)
		} else {
			*have = *c
			c = have
		}
		f.cards[c.ID] = c
		changed = true
	}
	if z := strings.TrimSpace(given.Zone); z != "" && len(z) <= 64 && z != f.collection.Zone {
		f.collection.Zone = z
		changed = true
	}
	if changed {
		f.collection.USN = usn
		if err := f.save(); err != nil {
			// What is in memory is ahead of the file; read it again next time.
			f.loaded = false
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	out := flashCollection{USN: f.collection.USN, Zone: f.collection.Zone, Decks: []*flashDeck{}, Cards: []*flashCard{}}
	sent := map[string]bool{}
	for _, d := range f.collection.Decks {
		if d.USN > since {
			out.Decks = append(out.Decks, d)
			sent[d.ID] = true
		}
	}
	for _, d := range lostDecks {
		if !sent[d.ID] {
			out.Decks = append(out.Decks, d)
		}
	}
	sent = map[string]bool{}
	for _, c := range f.collection.Cards {
		if c.USN > since {
			out.Cards = append(out.Cards, c)
			sent[c.ID] = true
		}
	}
	for _, c := range lostCards {
		if !sent[c.ID] {
			out.Cards = append(out.Cards, c)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func checkFlashDeck(d *flashDeck) string {
	if d == nil || d.ID == "" || len(d.ID) > maxFlashcardID {
		return "a deck needs an id"
	}
	if strings.TrimSpace(d.Name) == "" || len(d.Name) > maxFlashcardText {
		return "a deck needs a name"
	}
	if d.NewPerDay < -1 || d.NewPerDay > 9999 {
		return "new cards per day is -1 (auto) or 0 to 9999"
	}
	return ""
}

func checkFlashCard(c *flashCard) string {
	if c == nil || c.ID == "" || len(c.ID) > maxFlashcardID || c.Deck == "" || len(c.Deck) > maxFlashcardID {
		return "a card needs an id and a deck"
	}
	if len(c.Front) > maxFlashcardText || len(c.Back) > maxFlashcardText || len(c.Aux) > maxFlashcardText {
		return fmt.Sprintf("a card's text is at most %d bytes a side", maxFlashcardText)
	}
	if !utf8.ValidString(c.Front + c.Back + c.Aux + c.ID + c.Deck) {
		return "a card's text must be UTF-8"
	}
	if c.State < 1 || c.State > 3 {
		return "a card's state is 1 (learning), 2 (review) or 3 (relearning)"
	}
	return ""
}

// load reads the collection the first time it is needed.
func (f *flashcardService) load() error {
	if f.loaded {
		return nil
	}
	c := flashCollection{}
	data, err := os.ReadFile(f.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(data, &c); err != nil {
			return fmt.Errorf("malformed %s: %v", flashcardsFile, err)
		}
	}
	if c.Decks == nil {
		c.Decks = []*flashDeck{}
	}
	if c.Cards == nil {
		c.Cards = []*flashCard{}
	}
	f.collection = c
	f.decks = map[string]*flashDeck{}
	f.cards = map[string]*flashCard{}
	for _, d := range c.Decks {
		f.decks[d.ID] = d
	}
	for _, card := range c.Cards {
		f.cards[card.ID] = card
	}
	f.loaded = true
	return nil
}

func (f *flashcardService) save() error {
	out, err := json.Marshal(f.collection)
	if err != nil {
		return err
	}
	return writePrivateFile(f.path, append(out, '\n'))
}

// Pairing

func (f *flashcardService) startPairing(w http.ResponseWriter, r *http.Request) {
	var given struct {
		Name string `json:"name"`
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&given)
	name := strings.TrimSpace(given.Name)
	if utf8.RuneCountInString(name) > 60 || !utf8.ValidString(name) {
		name = ""
	}
	f.devMu.Lock()
	defer f.devMu.Unlock()
	f.prune()
	if len(f.pairings) >= maxFlashcardPairings {
		writeError(w, http.StatusTooManyRequests, "too many pairings are waiting; try again in a few minutes")
		return
	}
	p := &flashPairing{name: name, secret: randomHex(24), expires: time.Now().Add(flashcardPairLifetime)}
	for {
		p.code = randomCode(6)
		if f.pairing(p.code) == nil {
			break
		}
	}
	f.pairings = append(f.pairings, p)
	writeJSON(w, http.StatusCreated, map[string]any{"code": p.code[:3] + "-" + p.code[3:], "secret": p.secret, "expires": p.expires.UTC()})
}

func (f *flashcardService) pollPairing(w http.ResponseWriter, r *http.Request) {
	secret := r.PathValue("secret")
	f.devMu.Lock()
	defer f.devMu.Unlock()
	f.prune()
	for i, p := range f.pairings {
		if subtle.ConstantTimeCompare([]byte(p.secret), []byte(secret)) != 1 {
			continue
		}
		if p.token == "" {
			writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
			return
		}
		f.pairings = append(f.pairings[:i], f.pairings[i+1:]...)
		writeJSON(w, http.StatusOK, map[string]string{"status": "paired", "token": p.token})
		return
	}
	writeError(w, http.StatusNotFound, "no such pairing (it may have expired)")
}

func (f *flashcardService) approve(w http.ResponseWriter, r *http.Request) {
	var given struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&given); err != nil {
		writeError(w, http.StatusBadRequest, `expected {"code", "name"?}`)
		return
	}
	code := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(given.Code))
	name := strings.TrimSpace(given.Name)
	if utf8.RuneCountInString(name) > 60 {
		writeError(w, http.StatusBadRequest, "a device's name is at most 60 characters")
		return
	}
	f.devMu.Lock()
	defer f.devMu.Unlock()
	f.prune()
	p := f.pairing(code)
	if p == nil || p.token != "" {
		writeError(w, http.StatusNotFound, "no device is showing that code")
		return
	}
	if name == "" {
		name = p.name
	}
	if name == "" {
		name = "Device"
	}
	devices, err := f.loadDevices()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	token := flashcardTokenStart + randomHex(24)
	sum := sha256.Sum256([]byte(token))
	now := time.Now().UTC()
	d := flashDevice{ID: randomHex(8), Name: name, Hash: hex.EncodeToString(sum[:]), Added: now}
	devices = append(devices, d)
	if err := f.saveDevices(devices); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.token = token
	d.Hash = ""
	writeJSON(w, http.StatusCreated, d)
}

func (f *flashcardService) listDevices(w http.ResponseWriter, r *http.Request) {
	f.devMu.Lock()
	defer f.devMu.Unlock()
	devices, err := f.loadDevices()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for i := range devices {
		devices[i].Hash = ""
	}
	writeJSON(w, http.StatusOK, devices)
}

func (f *flashcardService) removeDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f.devMu.Lock()
	defer f.devMu.Unlock()
	devices, err := f.loadDevices()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	kept := devices[:0]
	for _, d := range devices {
		if d.ID != id {
			kept = append(kept, d)
		}
	}
	if len(kept) == len(devices) {
		writeError(w, http.StatusNotFound, "no such device")
		return
	}
	if err := f.saveDevices(kept); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"removed": id})
}

// validToken says whether token is a paired device's, and notes when it
// was last seen.
func (f *flashcardService) validToken(token string) bool {
	if !strings.HasPrefix(token, flashcardTokenStart) {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	want := hex.EncodeToString(sum[:])
	f.devMu.Lock()
	defer f.devMu.Unlock()
	devices, err := f.loadDevices()
	if err != nil {
		return false
	}
	for i, d := range devices {
		if subtle.ConstantTimeCompare([]byte(d.Hash), []byte(want)) == 1 {
			// Only a few times a day: every sync needn't rewrite the file.
			if time.Since(d.Seen) > time.Hour {
				devices[i].Seen = time.Now().UTC()
				f.saveDevices(devices)
			}
			return true
		}
	}
	return false
}

func (f *flashcardService) pairing(code string) *flashPairing {
	for _, p := range f.pairings {
		if p.code == code {
			return p
		}
	}
	return nil
}

func (f *flashcardService) prune() {
	now := time.Now()
	kept := f.pairings[:0]
	for _, p := range f.pairings {
		if now.Before(p.expires) {
			kept = append(kept, p)
		}
	}
	f.pairings = kept
}

func (f *flashcardService) loadDevices() ([]flashDevice, error) {
	data, err := os.ReadFile(f.devicesPath)
	if errors.Is(err, os.ErrNotExist) {
		return []flashDevice{}, nil
	} else if err != nil {
		return nil, err
	}
	var devices []flashDevice
	if err := json.Unmarshal(data, &devices); err != nil {
		return nil, fmt.Errorf("malformed %s: %v", flashcardDevicesFile, err)
	}
	if devices == nil {
		devices = []flashDevice{}
	}
	sort.SliceStable(devices, func(i, j int) bool { return devices[i].Added.Before(devices[j].Added) })
	return devices, nil
}

func (f *flashcardService) saveDevices(devices []flashDevice) error {
	out, err := json.MarshalIndent(devices, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(f.devicesPath, append(out, '\n'))
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func randomCode(n int) string {
	out := make([]byte, n)
	max := big.NewInt(int64(len(flashcardCodeAlphabet)))
	for i := range out {
		k, _ := rand.Int(rand.Reader, max)
		out[i] = flashcardCodeAlphabet[k.Int64()]
	}
	return string(out)
}
