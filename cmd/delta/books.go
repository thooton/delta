package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// `delta books` searches for and downloads ebooks. The book source's login
// lives on the command center (GET/PUT/DELETE /books-login); the CLI fetches
// it only when it has to log in, and keeps the session it gets, with the last
// search and its results, in books-state.json beside the CLI config. Logins
// are rate-limited by the source, so the session is reused for as long as it
// works.
//
// The agent never sees where books come from: errors go through booksError,
// and nothing printed names the source or its hosts.

// booksState is everything carried between runs.
type booksState struct {
	Email      string      `json:"email,omitempty"` // the account the session belongs to
	Domain     string      `json:"domain,omitempty"`
	UserID     string      `json:"remix_userid,omitempty"`
	UserKey    string      `json:"remix_userkey,omitempty"`
	Search     *bookSearch `json:"search,omitempty"`
	TotalPages int         `json:"total_pages,omitempty"`
	Results    []bookInfo  `json:"results,omitempty"`
}

func booksStatePath() (string, error) {
	path, err := configPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "books-state.json"), nil
}

func loadBooksState() (*booksState, error) {
	var s booksState
	path, err := booksStatePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &s, nil
	} else if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return &s, nil
}

// save writes the state atomically; it holds the session, so 0600.
func (s *booksState) save() error {
	path, err := booksStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(s, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func runBooks(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(booksHelp)
		return nil
	}
	cmd, args := args[0], args[1:]
	var err error
	switch cmd {
	case "search":
		err = booksSearchCmd(args)
	case "next", "prev", "page":
		err = booksPageCmd(cmd, args)
	case "results":
		err = booksResultsCmd(args)
	case "download", "get":
		err = booksDownloadCmd(args)
	case "login":
		err = booksLoginCmd(args)
	default:
		return fmt.Errorf("unknown books command %q (run `delta books help`)", cmd)
	}
	return booksError(err)
}

// booksSession is a logged-in client. A session loaded from the state file
// may have expired; try logs in again once when a call fails with one.
type booksSession struct {
	st    *booksState
	c     *booksClient
	fresh bool
}

func newBooksSession(st *booksState) (*booksSession, error) {
	if st.UserID == "" || st.UserKey == "" {
		b := &booksSession{st: st}
		return b, b.login()
	}
	domain := st.Domain
	if d := pinnedBooksDomain(); d != "" {
		domain = d
	}
	return &booksSession{st: st, c: newBooksClient(domain, st.UserID, st.UserKey)}, nil
}

// login fetches the account from the command center and logs in: resolve a
// healthy domain, log in, then switch to a healthy advertised domain.
func (b *booksSession) login() error {
	acct, err := fetchBooksLogin()
	if err != nil {
		return err
	}
	c, err := booksLogin(acct)
	if err != nil {
		return err
	}
	b.c, b.fresh = c, true
	b.st.Email, b.st.Domain, b.st.UserID, b.st.UserKey = acct.Email, c.Domain, c.UserID, c.UserKey
	return b.st.save()
}

func booksLogin(acct booksAccount) (*booksClient, error) {
	domain := resolveBooksDomain()
	c := newBooksClient(domain, "", "")
	if err := c.login(acct.Email, acct.Password); err != nil {
		return nil, err
	}
	if pinnedBooksDomain() == "" {
		if domains, err := c.domains(); err == nil {
			if target := selectAdvertisedDomain(domains, domain); target != "" && target != domain {
				c = newBooksClient(target, c.UserID, c.UserKey)
			}
		}
	}
	return c, nil
}

// try runs fn, logging in again and retrying once if it fails with a stored
// session that may have expired.
func (b *booksSession) try(fn func(*booksClient) error) error {
	err := fn(b.c)
	if err == nil || b.fresh || isBooksNetError(err) {
		return err
	}
	if lerr := b.login(); lerr != nil {
		return fmt.Errorf("%v (logging in again also failed: %v)", err, lerr)
	}
	return fn(b.c)
}

type booksAccount struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func fetchBooksLogin() (booksAccount, error) {
	var a booksAccount
	cfg, err := loadConfig()
	if err != nil {
		return a, err
	}
	data, err := do(cfg, http.MethodGet, "/books-login", nil)
	if err != nil {
		return a, fmt.Errorf("fetching the book login from the command center: %w", err)
	}
	if err := json.Unmarshal(data, &a); err != nil {
		return a, err
	}
	return a, nil
}

func booksSearchCmd(args []string) error {
	o, err := parseOpts(args, "n limit page ext format lang from to order", "exact")
	if err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(o.pos, " "))
	if query == "" {
		return errors.New("usage: delta books search <query> [options]")
	}
	p := bookSearch{Query: query, Exact: o.bools["exact"], Order: o.str("order")}
	if p.Limit, err = o.int("limit", 0); err == nil && p.Limit == 0 {
		p.Limit, err = o.int("n", 10)
	}
	if err != nil {
		return err
	}
	if p.Page, err = o.int("page", 1); err != nil {
		return err
	}
	if p.YearFrom, err = o.int("from", 0); err != nil {
		return err
	}
	if p.YearTo, err = o.int("to", 0); err != nil {
		return err
	}
	switch p.Order {
	case "", "popular", "date_created", "date_updated":
	default:
		return fmt.Errorf("unknown --order %q (popular, date_created or date_updated)", p.Order)
	}
	if p.Limit < 1 || p.Page < 1 {
		return errors.New("--limit and --page must be at least 1")
	}
	p.Extensions = splitList(firstNonEmpty(o.str("ext"), o.str("format")), true)
	p.Languages = splitList(o.str("lang"), false)
	st, err := loadBooksState()
	if err != nil {
		return err
	}
	return booksSearchAndSave(st, p, o.bools["json"])
}

func booksPageCmd(cmd string, args []string) error {
	o, err := parseOpts(args, "", "")
	if err != nil {
		return err
	}
	st, err := loadBooksState()
	if err != nil {
		return err
	}
	if st.Search == nil {
		return errors.New("no search yet; run `delta books search <query>` first")
	}
	p := *st.Search
	switch cmd {
	case "next":
		p.Page++
	case "prev":
		p.Page--
	case "page":
		if len(o.pos) != 1 {
			return errors.New("usage: delta books page <n>")
		}
		if p.Page, err = strconv.Atoi(o.pos[0]); err != nil {
			return fmt.Errorf("invalid page number %q", o.pos[0])
		}
	}
	if cmd != "page" && len(o.pos) != 0 {
		return fmt.Errorf("usage: delta books %s", cmd)
	}
	if p.Page < 1 || p.Page > st.TotalPages {
		return fmt.Errorf("page %d is out of range (1-%d)", p.Page, st.TotalPages)
	}
	return booksSearchAndSave(st, p, o.bools["json"])
}

func booksSearchAndSave(st *booksState, p bookSearch, asJSON bool) error {
	b, err := newBooksSession(st)
	if err != nil {
		return err
	}
	var r *bookSearchResult
	err = b.try(func(c *booksClient) error {
		r, err = c.search(p)
		if err == nil && !r.Success.ok() {
			err = errors.New(firstNonEmpty(string(r.Error), "the search failed"))
		}
		return err
	})
	if err != nil {
		return err
	}
	st.Search, st.TotalPages, st.Results = &p, r.pages(), r.Books
	if err := st.save(); err != nil {
		return err
	}
	return printBookResults(st, asJSON)
}

func booksResultsCmd(args []string) error {
	o, err := parseOpts(args, "", "")
	if err != nil {
		return err
	}
	if len(o.pos) != 0 {
		return errors.New("usage: delta books results [--json]")
	}
	st, err := loadBooksState()
	if err != nil {
		return err
	}
	return printBookResults(st, o.bools["json"])
}

func printBookResults(st *booksState, asJSON bool) error {
	if st.Search == nil {
		fmt.Println("No search yet; run `delta books search <query>`.")
		return nil
	}
	if asJSON {
		type result struct {
			Number    int    `json:"number"`
			Title     string `json:"title"`
			Author    string `json:"author,omitempty"`
			Year      string `json:"year,omitempty"`
			Language  string `json:"language,omitempty"`
			Format    string `json:"format,omitempty"`
			Size      string `json:"size,omitempty"`
			Publisher string `json:"publisher,omitempty"`
			Pages     string `json:"pages,omitempty"`
		}
		out := struct {
			Query      string   `json:"query"`
			Page       int      `json:"page"`
			TotalPages int      `json:"total_pages"`
			Results    []result `json:"results"`
		}{st.Search.Query, st.Search.Page, st.TotalPages, []result{}}
		for i, b := range st.Results {
			out.Results = append(out.Results, result{
				Number: i + 1, Title: string(b.Title), Author: string(b.Author), Year: nonZero(b.Year),
				Language: string(b.Language), Format: strings.ToLower(string(b.Extension)), Size: bookSize(b),
				Publisher: string(b.Publisher), Pages: nonZero(b.Pages),
			})
		}
		return printJSON(out)
	}
	fmt.Printf("Results for %q, page %d of %d\n", st.Search.Query, st.Search.Page, st.TotalPages)
	if len(st.Results) == 0 {
		fmt.Println("No results.")
		return nil
	}
	for i, b := range st.Results {
		fmt.Printf("%3d. %s\n", i+1, shorten(string(b.Title), 90))
		var meta []string
		for _, v := range []string{string(b.Author), nonZero(b.Year), string(b.Language), strings.ToUpper(string(b.Extension)), bookSize(b)} {
			if v != "" {
				meta = append(meta, v)
			}
		}
		fmt.Printf("     %s\n", shorten(strings.Join(meta, " · "), 100))
	}
	return nil
}

func nonZero(f flexString) string {
	if s := strings.TrimSpace(string(f)); s != "0" {
		return s
	}
	return ""
}

func bookSize(b bookInfo) string {
	if s := string(b.FilesizeString); s != "" {
		return s
	}
	if n, err := strconv.ParseInt(string(b.Filesize), 10, 64); err == nil && n > 0 {
		return humanSize(n)
	}
	return nonZero(b.Filesize)
}

func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func splitList(s string, upper bool) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			if upper {
				v = strings.ToUpper(v)
			} else {
				v = strings.ToLower(v)
			}
			out = append(out, v)
		}
	}
	return out
}

func booksDownloadCmd(args []string) error {
	o, err := parseOpts(args, "o output-dir", "push")
	if err != nil {
		return err
	}
	if len(o.pos) == 0 {
		return errors.New("usage: delta books download <number>... [-o dir] [--push]")
	}
	st, err := loadBooksState()
	if err != nil {
		return err
	}
	var books []bookInfo
	for _, a := range o.pos {
		for _, f := range strings.Split(a, ",") {
			if f == "" {
				continue
			}
			i, err := strconv.Atoi(f)
			if err != nil || i < 1 || i > len(st.Results) {
				if len(st.Results) == 0 {
					return errors.New("no results to download; run `delta books search <query>` first")
				}
				return fmt.Errorf("invalid result number %q (the last search has 1-%d)", f, len(st.Results))
			}
			books = append(books, st.Results[i-1])
		}
	}
	outDir, err := filepath.Abs(expandHome(firstNonEmpty(o.str("output-dir"), o.str("o"), ".")))
	if err != nil {
		return err
	}
	b, err := newBooksSession(st)
	if err != nil {
		return err
	}
	failed := 0
	for _, bk := range books {
		fmt.Fprintf(os.Stderr, "Downloading %q...\n", shorten(string(bk.Title), 70))
		var dest string
		err := b.try(func(c *booksClient) (err error) {
			dest, err = downloadBook(c, bk, outDir)
			return err
		})
		if err == nil && o.bools["push"] {
			err = push(dest, filepath.Base(dest))
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Failed: %v\n", booksError(err))
			failed++
			continue
		}
		fmt.Println(dest)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d downloads failed", failed, len(books))
	}
	return nil
}

func downloadBook(c *booksClient, b bookInfo, outDir string) (string, error) {
	link, err := c.downloadLink(string(b.ID), string(b.Hash))
	if err != nil {
		return "", fmt.Errorf("could not get the download link: %w", err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	resp, body, cancel, err := c.openDownload(link)
	if err != nil {
		return "", err
	}
	defer cancel()
	if strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		return "", errors.New("got a web page instead of the file (the account's daily download limit may be used up)")
	}

	tmp, err := os.CreateTemp(outDir, ".delta-book-*.part")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	tmp.Chmod(0o644)
	n, err := io.Copy(tmp, body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if errors.Is(err, context.Canceled) {
		return "", errors.New("the download stalled")
	} else if err != nil {
		// Read errors can carry the server's address.
		var pe *os.PathError
		if errors.As(err, &pe) {
			return "", err
		}
		return "", errors.New("the download was interrupted")
	}
	if n == 0 {
		return "", errors.New("the download was empty")
	}

	ext := string(b.Extension)
	if normalizeExtension(ext) == "" {
		ext = filepath.Ext(responseFilename(resp.Header.Get("Content-Disposition"), resp.Request.URL))
	}
	dest := filepath.Join(outDir, bookFilename(b, ext))
	if err := os.Link(tmp.Name(), dest); err != nil {
		if errors.Is(err, os.ErrExist) {
			if st, serr := os.Stat(dest); serr == nil && st.Size() == n {
				return dest, nil // the same book was already downloaded here
			}
			return "", fmt.Errorf("refusing to replace the existing %s", dest)
		}
		if err := os.Rename(tmp.Name(), dest); err != nil {
			return "", err
		}
	}
	return dest, nil
}

func booksLoginCmd(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	sub := "show"
	if len(args) > 0 {
		sub = args[0]
	}
	switch {
	case sub == "show" && len(args) <= 1:
		acct, err := fetchBooksLogin()
		if err != nil {
			return err
		}
		fmt.Printf("Book account: %s\n", acct.Email)
		return nil
	case sub == "set" && len(args) == 3:
		acct := booksAccount{Email: strings.TrimSpace(args[1]), Password: args[2]}
		if acct.Password == "-" {
			line, err := bufio.NewReader(os.Stdin).ReadString('\n')
			if err != nil && line == "" {
				return fmt.Errorf("reading the password from stdin: %w", err)
			}
			acct.Password = strings.TrimRight(line, "\r\n")
		}
		if !strings.Contains(acct.Email, "@") || acct.Password == "" {
			return errors.New("the login needs an email address and a password")
		}
		// Try it first, so a typo isn't saved. If the source can't be reached
		// from here, save it anyway.
		st, err := loadBooksState()
		if err != nil {
			return err
		}
		c, lerr := booksLogin(acct)
		if lerr != nil && !isBooksNetError(lerr) {
			return fmt.Errorf("not saved: %w", lerr)
		}
		data, _ := json.Marshal(acct)
		if _, err := do(cfg, http.MethodPut, "/books-login", bytes.NewReader(data)); err != nil {
			return err
		}
		st.Email, st.Domain, st.UserID, st.UserKey = acct.Email, "", "", ""
		if lerr == nil {
			st.Domain, st.UserID, st.UserKey = c.Domain, c.UserID, c.UserKey
		}
		if err := st.save(); err != nil {
			return err
		}
		if lerr != nil {
			fmt.Printf("Book account %s saved on the command center, but it couldn't be checked: %v\n", acct.Email, booksError(lerr))
		} else {
			fmt.Printf("Logged in as %s; saved on the command center.\n", acct.Email)
		}
		return nil
	case sub == "remove" && len(args) == 1:
		if _, err := do(cfg, http.MethodDelete, "/books-login", nil); err != nil {
			return err
		}
		if st, err := loadBooksState(); err == nil {
			st.Email, st.Domain, st.UserID, st.UserKey = "", "", "", ""
			st.save()
		}
		fmt.Println("Book account removed from the command center.")
		return nil
	default:
		return errors.New("usage: delta books login [show | set <email> <password|-> | remove]")
	}
}

// Download naming, as the reference does it:
// {AuthorCamelCase}_{TitleCamelCase}[_{Year}][_{Lang}]_{BookID}.{ext}

var bookExtensions = map[string]bool{
	"txt": true, "pdf": true, "fb2": true, "epub": true, "lit": true, "mobi": true,
	"rtf": true, "djv": true, "djvu": true, "azw": true, "azw3": true,
	"cbr": true, "cbz": true, "doc": true, "docx": true, "odt": true,
}

func normalizeExtension(ext string) string {
	n := strings.ToLower(strings.TrimLeft(strings.TrimSpace(ext), "."))
	if bookExtensions[n] {
		return n
	}
	return ""
}

// asciiFold maps accented Latin letters to their base letter. The reference
// NFKD-decomposes and drops what isn't ASCII; this covers the Latin-1 and
// Latin Extended-A letters that decompose, without leaving the standard
// library. Anything else not ASCII is dropped, as there.
var asciiFold = func() map[rune]rune {
	m := map[rune]rune{}
	for _, g := range strings.Fields("AÀÁÂÃÄÅĀĂĄ aàáâãäåāăą CÇĆĈĊČ cçćĉċč DĎ dď EÈÉÊËĒĔĖĘĚ eèéêëēĕėęě " +
		"GĜĞĠĢ gĝğġģ HĤ hĥ IÌÍÎÏĨĪĬĮİ iìíîïĩīĭį JĴ jĵ KĶ kķ LĹĻĽĿ lĺļľŀ NÑŃŅŇ nñńņň " +
		"OÒÓÔÕÖŌŎŐ oòóôõöōŏő RŔŖŘ rŕŗř SŚŜŞŠ sśŝşš TŢŤ tţť UÙÚÛÜŨŪŬŮŰŲ uùúûüũūŭůűų " +
		"WŴ wŵ YÝŶŸ yýÿŷ ZŹŻŽ zźżž") {
		r := []rune(g)
		for _, v := range r[1:] {
			m[v] = r[0]
		}
	}
	return m
}()

var wordRe = regexp.MustCompile(`[A-Za-z0-9]+`)

func toCamelCase(value string, maxLen int) string {
	var ascii strings.Builder
	for _, r := range strings.TrimSpace(value) {
		if f, ok := asciiFold[r]; ok {
			r = f
		}
		if r < 128 {
			ascii.WriteRune(r)
		}
	}
	var b strings.Builder
	for _, w := range wordRe.FindAllString(ascii.String(), -1) {
		b.WriteString(strings.ToUpper(w[:1]) + strings.ToLower(w[1:]))
	}
	camel := b.String()
	if maxLen > 0 && len(camel) > maxLen {
		camel = camel[:maxLen]
	}
	if camel == "" {
		return "Unknown"
	}
	return camel
}

// alnum keeps only ASCII letters and digits, so a value is safe in a file name.
func alnum(s string) string { return strings.Join(wordRe.FindAllString(s, -1), "") }

func formatAuthorCamelCase(author string) string {
	if strings.TrimSpace(author) == "" {
		return "UnknownAuthor"
	}
	if strings.Contains(author, ",") {
		parts := strings.Split(author, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		author = strings.Join(parts, " ")
	} else if parts := strings.Fields(author); len(parts) > 1 {
		author = parts[len(parts)-1] + " " + strings.Join(parts[:len(parts)-1], " ")
	}
	return toCamelCase(author, 50)
}

func bookFilename(b bookInfo, ext string) string {
	author := formatAuthorCamelCase(string(b.Author))
	title := "UntitledBook"
	if t := string(b.Title); t != "" {
		title = toCamelCase(t, 100)
	}
	id := strings.TrimSpace(string(b.ID))
	if id == "" || strings.ContainsAny(id, `/\`) {
		id = "NoID"
	}

	parts := []string{author, title}
	if y := alnum(nonZero(b.Year)); y != "" {
		parts = append(parts, y)
	}
	if l := []rune(strings.ToLower(strings.TrimSpace(string(b.Language)))); len(l) > 0 {
		if l := alnum(string(l[:min(5, len(l))])); l != "" {
			parts = append(parts, l)
		}
	}
	parts = append(parts, id)
	base := strings.Join(parts, "_")

	const maxTotal = 200
	if len(base) > maxTotal {
		avail := maxTotal - len(id) - 1
		authorMax := int(float64(avail) * 0.4)
		titleMax := avail - authorMax
		base = author[:min(authorMax, len(author))] + "_" + title[:min(titleMax, len(title))] + "_" + id
	}
	if e := normalizeExtension(ext); e != "" {
		return base + "." + e
	}
	return base
}
