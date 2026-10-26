package main

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The user's two-factor accounts: the time-based one-time password (TOTP)
// secrets that Authenticator on the phone shows codes for. The command center
// is where they live, so a phone that is lost or reset loses none of them;
// the app keeps a copy of the list so codes still show offline, and syncs it
// on every start. Codes are worked out on the phone: the command center only
// stores the secrets, in a JSON file readable only by its user.
//
// Routes (all behind the delta bearer token, and not open to backup sessions):
//
//	GET    /otp        The accounts, oldest first, as [{"id", "issuer",
//	                   "account", "secret", "algorithm", "digits", "period",
//	                   "added"}].
//	POST   /otp        Adds one account ({"issuer", "account", "secret",
//	                   "algorithm"?, "digits"?, "period"?}) or a list of them
//	                   (a Google Authenticator export carries several). An
//	                   account whose secret is already there is left as it
//	                   is. Answers 201 with every account, as GET does.
//	PUT    /otp/{id}   Changes one: the same fields as POST, with an empty
//	                   secret keeping the one it has. A secret another
//	                   account already has is refused. Answers with every
//	                   account.
//	DELETE /otp/{id}   Removes one, and answers with the accounts left.
//
// The secret is Base32 (RFC 4648), stored upper case without spaces or
// padding; algorithm is SHA1 (the default), SHA256 or SHA512; digits 6 (the
// default) to 8; period, in seconds, 30 by default.
const (
	otpFile        = "otp.json"
	maxOTPBody     = 256 << 10
	maxOTPAccounts = 1000
	maxOTPText     = 200
	maxOTPSecret   = 256 // bytes, once decoded
)

type otpAccount struct {
	ID        string    `json:"id"`
	Issuer    string    `json:"issuer"`
	Account   string    `json:"account"`
	Secret    string    `json:"secret"`
	Algorithm string    `json:"algorithm"`
	Digits    int       `json:"digits"`
	Period    int       `json:"period"`
	Added     time.Time `json:"added"`
}

type otpStore struct {
	path string
	mu   sync.Mutex
}

func (o *otpStore) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /otp", o.list)
	mux.HandleFunc("POST /otp", o.add)
	mux.HandleFunc("PUT /otp/{id}", o.update)
	mux.HandleFunc("DELETE /otp/{id}", o.remove)
}

func (o *otpStore) list(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	accounts, err := o.load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, accounts)
}

func (o *otpStore) add(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxOTPBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("accounts exceed %d bytes", maxOTPBody))
		return
	}
	var given []otpAccount
	if trimmed := strings.TrimSpace(string(data)); strings.HasPrefix(trimmed, "[") {
		err = json.Unmarshal(data, &given)
	} else {
		var one otpAccount
		err = json.Unmarshal(data, &one)
		given = []otpAccount{one}
	}
	if err != nil || len(given) == 0 {
		writeError(w, http.StatusBadRequest, `expected {"issuer", "account", "secret", "algorithm"?, "digits"?, "period"?} or a list of them`)
		return
	}
	for i := range given {
		if why := normalizeOTP(&given[i]); why != "" {
			writeError(w, http.StatusBadRequest, why)
			return
		}
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	accounts, err := o.load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	have := map[string]bool{}
	for _, a := range accounts {
		have[a.Secret] = true
	}
	now := time.Now().UTC()
	for _, a := range given {
		if have[a.Secret] {
			continue
		}
		if len(accounts) >= maxOTPAccounts {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("at most %d accounts are kept", maxOTPAccounts))
			return
		}
		id := make([]byte, 8)
		rand.Read(id)
		a.ID = hex.EncodeToString(id)
		a.Added = now
		accounts = append(accounts, a)
		have[a.Secret] = true
	}
	if err := o.save(accounts); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, accounts)
}

func (o *otpStore) update(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxOTPBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("an account exceeds %d bytes", maxOTPBody))
		return
	}
	var given otpAccount
	if err := json.Unmarshal(data, &given); err != nil {
		writeError(w, http.StatusBadRequest, `expected {"issuer", "account", "secret"?, "algorithm"?, "digits"?, "period"?}`)
		return
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	accounts, err := o.load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	at := -1
	for i, a := range accounts {
		if a.ID == id {
			at = i
		}
	}
	if at < 0 {
		writeError(w, http.StatusNotFound, "no such account")
		return
	}
	if strings.TrimSpace(given.Secret) == "" {
		given.Secret = accounts[at].Secret
	}
	if why := normalizeOTP(&given); why != "" {
		writeError(w, http.StatusBadRequest, why)
		return
	}
	for i, a := range accounts {
		if i != at && a.Secret == given.Secret {
			writeError(w, http.StatusBadRequest, "another account already has that secret key")
			return
		}
	}
	given.ID = accounts[at].ID
	given.Added = accounts[at].Added
	accounts[at] = given
	if err := o.save(accounts); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, accounts)
}

func (o *otpStore) remove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	o.mu.Lock()
	defer o.mu.Unlock()
	accounts, err := o.load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	kept := accounts[:0]
	found := false
	for _, a := range accounts {
		if a.ID == id {
			found = true
			continue
		}
		kept = append(kept, a)
	}
	if !found {
		writeError(w, http.StatusNotFound, "no such account")
		return
	}
	if err := o.save(kept); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, kept)
}

// load reads the accounts; none at all is an empty list, never null.
func (o *otpStore) load() ([]otpAccount, error) {
	data, err := os.ReadFile(o.path)
	if errors.Is(err, os.ErrNotExist) {
		return []otpAccount{}, nil
	} else if err != nil {
		return nil, err
	}
	var accounts []otpAccount
	if err := json.Unmarshal(data, &accounts); err != nil {
		return nil, fmt.Errorf("malformed %s: %v", otpFile, err)
	}
	if accounts == nil {
		accounts = []otpAccount{}
	}
	return accounts, nil
}

func (o *otpStore) save(accounts []otpAccount) error {
	out, err := json.MarshalIndent(accounts, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(o.path, append(out, '\n'))
}

// normalizeOTP fills in the defaults and tidies what was given, or says, for
// the person who typed it, what is wrong with it.
func normalizeOTP(a *otpAccount) string {
	a.Issuer = strings.TrimSpace(a.Issuer)
	a.Account = strings.TrimSpace(a.Account)
	if a.Issuer == "" && a.Account == "" {
		return "an account needs a name"
	}
	if utf8.RuneCountInString(a.Issuer) > maxOTPText || utf8.RuneCountInString(a.Account) > maxOTPText {
		return fmt.Sprintf("a name is at most %d characters", maxOTPText)
	}
	secret := strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "=", "", "\t", "").Replace(a.Secret))
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil || len(key) == 0 {
		return "the secret key isn't valid Base32"
	}
	if len(key) > maxOTPSecret {
		return "the secret key is too long"
	}
	a.Secret = secret
	switch strings.ToUpper(strings.ReplaceAll(a.Algorithm, "-", "")) {
	case "", "SHA1":
		a.Algorithm = "SHA1"
	case "SHA256":
		a.Algorithm = "SHA256"
	case "SHA512":
		a.Algorithm = "SHA512"
	default:
		return "the algorithm must be SHA1, SHA256 or SHA512"
	}
	if a.Digits == 0 {
		a.Digits = 6
	}
	if a.Digits < 6 || a.Digits > 8 {
		return "codes have 6 to 8 digits"
	}
	if a.Period == 0 {
		a.Period = 30
	}
	if a.Period < 1 || a.Period > 300 {
		return "a code lasts 1 to 300 seconds"
	}
	return ""
}
