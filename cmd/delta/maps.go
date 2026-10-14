package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// `delta maps` finds places with the Google Places API (New). The API key
// lives on the command center (GET/PUT /maps-key); the CLI fetches it on
// every run and calls Google itself, so nothing keeps a local copy.
//
// Field masks decide what Google bills: search asks for nothing beyond the
// Enterprise tier (it needs ratings and opening hours), and only `place`
// asks for reviews and the other Atmosphere fields.

var mapsAPI = "https://places.googleapis.com/v1"

const (
	// Text Search returns at most 20 places per request.
	maxMapsResults = 20
	// How far --near pulls results toward its point when --within isn't
	// given. It is a preference, not a limit.
	mapsBiasMeters = 5000
	metersPerMile  = 1609.344
)

var mapsSearchFields = []string{
	"places.id", "places.displayName", "places.primaryTypeDisplayName",
	"places.formattedAddress", "places.location", "places.rating",
	"places.userRatingCount", "places.priceLevel", "places.businessStatus",
	"places.utcOffsetMinutes", "places.currentOpeningHours.openNow",
	"places.currentOpeningHours.nextOpenTime", "places.currentOpeningHours.nextCloseTime",
	"nextPageToken",
}

var mapsPlaceFields = []string{
	"id", "displayName", "primaryTypeDisplayName", "formattedAddress", "location",
	"nationalPhoneNumber", "internationalPhoneNumber", "websiteUri", "googleMapsUri",
	"googleMapsLinks.directionsUri", "rating", "userRatingCount", "priceLevel", "priceRange",
	"businessStatus", "utcOffsetMinutes", "currentOpeningHours", "regularOpeningHours",
	"currentSecondaryOpeningHours", "editorialSummary", "generativeSummary.overview",
	"reviewSummary.text", "reviews", "paymentOptions", "parkingOptions",
	"accessibilityOptions", "fuelOptions",
	// The yes/no features below.
	"dineIn", "takeout", "delivery", "curbsidePickup", "reservable",
	"servesBreakfast", "servesBrunch", "servesLunch", "servesDinner", "servesDessert",
	"servesCoffee", "servesBeer", "servesWine", "servesCocktails", "servesVegetarianFood",
	"outdoorSeating", "liveMusic", "menuForChildren", "goodForChildren", "goodForGroups",
	"goodForWatchingSports", "allowsDogs", "restroom",
}

// placeFeatures labels the yes/no fields, in the order they are printed.
var placeFeatures = []struct{ field, label string }{
	{"dineIn", "dine-in"}, {"takeout", "takeout"}, {"delivery", "delivery"},
	{"curbsidePickup", "curbside pickup"}, {"reservable", "reservations"},
	{"servesBreakfast", "breakfast"}, {"servesBrunch", "brunch"}, {"servesLunch", "lunch"},
	{"servesDinner", "dinner"}, {"servesDessert", "dessert"}, {"servesCoffee", "coffee"},
	{"servesBeer", "beer"}, {"servesWine", "wine"}, {"servesCocktails", "cocktails"},
	{"servesVegetarianFood", "vegetarian food"}, {"outdoorSeating", "outdoor seating"},
	{"liveMusic", "live music"}, {"menuForChildren", "kids' menu"},
	{"goodForChildren", "good for kids"}, {"goodForGroups", "good for groups"},
	{"goodForWatchingSports", "good for watching sports"}, {"allowsDogs", "dogs allowed"},
	{"restroom", "restroom"},
}

type mapsText struct {
	Text string `json:"text"`
}

type latLng struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

type openingHours struct {
	OpenNow             *bool    `json:"openNow"`
	NextOpenTime        string   `json:"nextOpenTime"`
	NextCloseTime       string   `json:"nextCloseTime"`
	WeekdayDescriptions []string `json:"weekdayDescriptions"`
	SecondaryHoursType  string   `json:"secondaryHoursType"`
}

type money struct {
	CurrencyCode string `json:"currencyCode"`
	Units        string `json:"units"`
	Nanos        int    `json:"nanos"`
}

type place struct {
	ID                       string   `json:"id"`
	DisplayName              mapsText `json:"displayName"`
	PrimaryTypeDisplayName   mapsText `json:"primaryTypeDisplayName"`
	FormattedAddress         string   `json:"formattedAddress"`
	Location                 *latLng  `json:"location"`
	NationalPhoneNumber      string   `json:"nationalPhoneNumber"`
	InternationalPhoneNumber string   `json:"internationalPhoneNumber"`
	WebsiteURI               string   `json:"websiteUri"`
	GoogleMapsURI            string   `json:"googleMapsUri"`
	GoogleMapsLinks          struct {
		DirectionsURI string `json:"directionsUri"`
	} `json:"googleMapsLinks"`
	Rating                       float64                                `json:"rating"`
	UserRatingCount              int                                    `json:"userRatingCount"`
	PriceLevel                   string                                 `json:"priceLevel"`
	PriceRange                   *struct{ StartPrice, EndPrice *money } `json:"priceRange"`
	BusinessStatus               string                                 `json:"businessStatus"`
	UTCOffsetMinutes             *int                                   `json:"utcOffsetMinutes"`
	CurrentOpeningHours          *openingHours                          `json:"currentOpeningHours"`
	RegularOpeningHours          *openingHours                          `json:"regularOpeningHours"`
	CurrentSecondaryOpeningHours []openingHours                         `json:"currentSecondaryOpeningHours"`
	EditorialSummary             mapsText                               `json:"editorialSummary"`
	GenerativeSummary            struct{ Overview mapsText }            `json:"generativeSummary"`
	ReviewSummary                struct{ Text mapsText }                `json:"reviewSummary"`
	Reviews                      []struct {
		Rating            float64  `json:"rating"`
		RelativeTime      string   `json:"relativePublishTimeDescription"`
		Text              mapsText `json:"text"`
		AuthorAttribution struct {
			DisplayName string `json:"displayName"`
		} `json:"authorAttribution"`
	} `json:"reviews"`
	PaymentOptions       map[string]bool `json:"paymentOptions"`
	ParkingOptions       map[string]bool `json:"parkingOptions"`
	AccessibilityOptions map[string]bool `json:"accessibilityOptions"`
	FuelOptions          struct {
		FuelPrices []struct {
			Type       string `json:"type"`
			Price      money  `json:"price"`
			UpdateTime string `json:"updateTime"`
		} `json:"fuelPrices"`
	} `json:"fuelOptions"`
	// Every field, for the yes/no features.
	raw map[string]json.RawMessage
}

func runMaps(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(mapsHelp)
		return nil
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "search", "find":
		return mapsSearchCmd(args)
	case "place", "details":
		return mapsPlaceCmd(args)
	case "key":
		if len(args) != 2 || args[0] != "set" {
			return errors.New("usage: delta maps key set <key|->")
		}
		return setMapsKey(args[1])
	default:
		return fmt.Errorf("unknown maps command %q (run `delta maps help`)", cmd)
	}
}

// mapsSearch is one search, as given on the command line.
type mapsSearch struct {
	Query     string
	Near      string  // a place or "lat,lng"; empty for none
	Within    float64 // meters; 0 for no limit
	Closest   bool
	OpenNow   bool
	MinRating float64
	Limit     int
}

func mapsSearchCmd(args []string) error {
	o, err := parseOpts(args, "near within min-rating n limit", "closest open-now")
	if err != nil {
		return err
	}
	s := mapsSearch{
		Query:   strings.TrimSpace(strings.Join(o.pos, " ")),
		Near:    strings.TrimSpace(o.str("near")),
		Closest: o.bools["closest"],
		OpenNow: o.bools["open-now"],
	}
	if s.Query == "" {
		return errors.New("usage: delta maps search <query> [--near <place|lat,lng>] [--within <distance>] [--closest] [--open-now] [--min-rating N] [--limit N]")
	}
	if s.Limit, err = o.int("limit", 0); err == nil && s.Limit == 0 {
		s.Limit, err = o.int("n", 10)
	}
	if err != nil {
		return err
	}
	if s.Limit < 1 || s.Limit > maxMapsResults {
		return fmt.Errorf("--limit must be from 1 to %d", maxMapsResults)
	}
	if v := o.str("min-rating"); v != "" {
		if s.MinRating, err = strconv.ParseFloat(v, 64); err != nil || s.MinRating < 0 || s.MinRating > 5 {
			return errors.New("--min-rating must be a number from 0 to 5")
		}
	}
	if v := o.str("within"); v != "" {
		if s.Within, err = parseDistance(v); err != nil {
			return err
		}
	}
	if s.Near == "" && (s.Within > 0 || s.Closest) {
		return errors.New("--within and --closest need --near")
	}
	c, err := newMapsClient()
	if err != nil {
		return err
	}
	return c.search(os.Stdout, s, o.bools["json"])
}

func mapsPlaceCmd(args []string) error {
	o, err := parseOpts(args, "", "")
	if err != nil {
		return err
	}
	ref := strings.TrimSpace(strings.Join(o.pos, " "))
	if ref == "" {
		return errors.New("usage: delta maps place <place-id|name and location>")
	}
	c, err := newMapsClient()
	if err != nil {
		return err
	}
	id, ok := placeID(ref)
	if !ok {
		// A name: look it up and show the best match.
		p, err := c.findOne(ref)
		if err != nil {
			return err
		}
		id = p.ID
	}
	return c.details(os.Stdout, id, o.bools["json"])
}

// placeID reports whether s is a place ID (optionally "places/<id>"), as
// opposed to a name to search for. IDs are long and have no spaces.
func placeID(s string) (string, bool) {
	s = strings.TrimPrefix(s, "places/")
	if len(s) < 20 {
		return "", false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return "", false
		}
	}
	return s, true
}

var distanceRE = regexp.MustCompile(`^([0-9]*\.?[0-9]+)\s*(mi|miles?|km|m|meters?|ft|feet)?$`)

// parseDistance turns "2mi", "1.5 km", "500m" or "800ft" into meters. A bare
// number is miles.
func parseDistance(s string) (float64, error) {
	m := distanceRE.FindStringSubmatch(strings.ToLower(strings.TrimSpace(s)))
	if m == nil {
		return 0, fmt.Errorf("bad distance %q (try 2mi, 1.5km, 500m or 800ft)", s)
	}
	n, _ := strconv.ParseFloat(m[1], 64)
	switch m[2] {
	case "km":
		n *= 1000
	case "m", "meter", "meters":
	case "ft", "feet":
		n *= 0.3048
	default:
		n *= metersPerMile
	}
	if n <= 0 {
		return 0, fmt.Errorf("bad distance %q", s)
	}
	return n, nil
}

var latLngRE = regexp.MustCompile(`^\s*(-?[0-9]+(?:\.[0-9]+)?)\s*,\s*(-?[0-9]+(?:\.[0-9]+)?)\s*$`)

func parseLatLng(s string) (latLng, bool) {
	m := latLngRE.FindStringSubmatch(s)
	if m == nil {
		return latLng{}, false
	}
	lat, _ := strconv.ParseFloat(m[1], 64)
	lng, _ := strconv.ParseFloat(m[2], 64)
	if lat < -90 || lat > 90 || lng < -180 || lng > 180 {
		return latLng{}, false
	}
	return latLng{lat, lng}, true
}

// distance is the great-circle distance in meters.
func distance(a, b latLng) float64 {
	const r = 6371008.8
	rad := math.Pi / 180
	dLat := (b.Latitude - a.Latitude) * rad
	dLng := (b.Longitude - a.Longitude) * rad
	h := math.Pow(math.Sin(dLat/2), 2) + math.Cos(a.Latitude*rad)*math.Cos(b.Latitude*rad)*math.Pow(math.Sin(dLng/2), 2)
	return 2 * r * math.Asin(math.Min(1, math.Sqrt(h)))
}

// boundingBox is the smallest latitude/longitude rectangle around the
// circle, in the shape Text Search's locationRestriction takes. Longitudes
// past ±180 wrap, which Google reads as a box crossing the antimeridian.
func boundingBox(center latLng, meters float64) map[string]latLng {
	dLat := meters / 111320
	dLng := 180.0
	if c := math.Cos(center.Latitude * math.Pi / 180); c > 1e-6 {
		dLng = math.Min(180, meters/(111320*c))
	}
	wrap := func(lng float64) float64 {
		if dLng >= 180 {
			return math.Max(-180, math.Min(180, lng))
		}
		return math.Mod(lng+540, 360) - 180
	}
	return map[string]latLng{
		"low":  {math.Max(-90, center.Latitude-dLat), wrap(center.Longitude - dLng)},
		"high": {math.Min(90, center.Latitude+dLat), wrap(center.Longitude + dLng)},
	}
}

func formatDistance(meters float64) string {
	mi := meters / metersPerMile
	switch {
	case mi < 0.1:
		return fmt.Sprintf("%.0f ft", meters/0.3048)
	case mi < 10:
		return fmt.Sprintf("%.1f mi", mi)
	default:
		return fmt.Sprintf("%.0f mi", mi)
	}
}

// mapsClient calls the Places API with the user's key.
type mapsClient struct {
	key  string
	http *http.Client
	now  func() time.Time
}

func newMapsClient() (*mapsClient, error) {
	key, err := fetchMapsKey()
	if err != nil {
		return nil, err
	}
	return &mapsClient{key: key, http: &http.Client{Timeout: 30 * time.Second}, now: time.Now}, nil
}

func fetchMapsKey() (string, error) {
	cfg, err := loadConfig()
	if err != nil {
		return "", err
	}
	data, err := do(cfg, http.MethodGet, "/maps-key", nil)
	if err != nil {
		return "", err
	}
	var res struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return "", err
	}
	return res.Key, nil
}

// mapsAPIError is an error response from Google.
type mapsAPIError struct {
	HTTPStatus int
	Status     string
	Message    string
	Reason     string
}

func (e *mapsAPIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d", e.HTTPStatus)
	}
	if e.Status != "" {
		msg += " (" + e.Status + ")"
	}
	return "Google Maps: " + msg
}

// call sends a request to the Places API, asking for fields, and decodes the
// response into out. It returns the raw response too, for --json.
func (c *mapsClient) call(method, path string, fields []string, body, out any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, mapsAPI+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Goog-Api-Key", c.key)
	req.Header.Set("X-Goog-FieldMask", strings.Join(fields, ","))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// Don't print the URL, which may carry the query.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return nil, fmt.Errorf("Google Maps can't be reached: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Message string `json:"message"`
				Status  string `json:"status"`
				Details []struct {
					Reason string `json:"reason"`
				} `json:"details"`
			} `json:"error"`
		}
		json.Unmarshal(data, &e)
		apiErr := &mapsAPIError{HTTPStatus: resp.StatusCode, Status: e.Error.Status, Message: strings.TrimSpace(e.Error.Message)}
		for _, d := range e.Error.Details {
			if d.Reason != "" {
				apiErr.Reason = d.Reason
			}
		}
		return nil, apiErr
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, fmt.Errorf("unexpected response from Google Maps: %w", err)
		}
	}
	return data, nil
}

// findOne returns the best match for a text query.
func (c *mapsClient) findOne(query string) (*place, error) {
	var res struct{ Places []place }
	fields := []string{"places.id", "places.displayName", "places.formattedAddress", "places.location"}
	if _, err := c.call(http.MethodPost, "/places:searchText", fields, map[string]any{"textQuery": query, "pageSize": 1}, &res); err != nil {
		return nil, err
	}
	if len(res.Places) == 0 || res.Places[0].ID == "" {
		return nil, fmt.Errorf("no place found for %q; try adding a city or address", query)
	}
	return &res.Places[0], nil
}

func (c *mapsClient) search(w io.Writer, s mapsSearch, asJSON bool) error {
	req := map[string]any{"textQuery": s.Query, "pageSize": s.Limit}
	if s.OpenNow {
		req["openNow"] = true
	}
	if s.MinRating > 0 {
		req["minRating"] = s.MinRating
	}
	var center *latLng
	var anchor string
	if s.Near != "" {
		if ll, ok := parseLatLng(s.Near); ok {
			center, anchor = &ll, fmt.Sprintf("%.5f, %.5f", ll.Latitude, ll.Longitude)
		} else {
			p, err := c.findOne(s.Near)
			if err != nil {
				return fmt.Errorf("--near: %w", err)
			}
			if p.Location == nil {
				return fmt.Errorf("--near: %q has no location", s.Near)
			}
			center, anchor = p.Location, p.DisplayName.Text
			if strings.HasPrefix(p.FormattedAddress, anchor) {
				anchor = p.FormattedAddress
			} else if p.FormattedAddress != "" {
				anchor += ", " + p.FormattedAddress
			}
		}
		if s.Within > 0 {
			req["locationRestriction"] = map[string]any{"rectangle": boundingBox(*center, s.Within)}
		} else {
			req["locationBias"] = map[string]any{"circle": map[string]any{"center": center, "radius": mapsBiasMeters}}
		}
		if s.Closest {
			req["rankPreference"] = "DISTANCE"
		}
	}
	var res struct {
		Places        []place `json:"places"`
		NextPageToken string  `json:"nextPageToken"`
	}
	raw, err := c.call(http.MethodPost, "/places:searchText", mapsSearchFields, req, &res)
	if err != nil {
		return err
	}
	// The restriction is a box; keep what is inside the circle.
	if s.Within > 0 {
		kept := res.Places[:0]
		for _, p := range res.Places {
			if p.Location == nil || distance(*center, *p.Location) <= s.Within {
				kept = append(kept, p)
			}
		}
		res.Places = kept
	}
	if asJSON {
		if s.Within > 0 {
			raw, _ = json.Marshal(map[string]any{"places": rawPlaces(raw, res.Places)})
		}
		return writeIndentedJSON(w, raw)
	}

	if anchor != "" {
		fmt.Fprintf(w, "Near: %s", anchor)
		if s.Within > 0 {
			fmt.Fprintf(w, " (within %s)", formatDistance(s.Within))
		}
		fmt.Fprintln(w)
	}
	if len(res.Places) == 0 {
		fmt.Fprintln(w, "No places found.")
		return nil
	}
	if loc := placeZone(&res.Places[0]); loc != nil {
		fmt.Fprintf(w, "Local time there: %s\n", c.now().In(loc).Format("Mon Jan 2, 3:04 PM"))
	}
	for i, p := range res.Places {
		fmt.Fprintf(w, "\n%d. %s\n", i+1, p.DisplayName.Text)
		if line := ratingLine(&p); line != "" {
			fmt.Fprintf(w, "   %s\n", line)
		}
		addr := p.FormattedAddress
		if center != nil && p.Location != nil {
			addr += " · " + formatDistance(distance(*center, *p.Location)) + " away"
		}
		fmt.Fprintf(w, "   %s\n", addr)
		if st := c.status(&p); st != "" {
			fmt.Fprintf(w, "   %s\n", st)
		}
		fmt.Fprintf(w, "   id: %s\n", p.ID)
	}
	fmt.Fprintln(w)
	if res.NextPageToken != "" && s.Limit < maxMapsResults {
		fmt.Fprintf(w, "More results exist (--limit goes up to %d).\n", maxMapsResults)
	}
	fmt.Fprintln(w, "Full details, hours and reviews: delta maps place <id>")
	return nil
}

// rawPlaces picks the kept places out of a raw search response, so --json
// prints Google's own objects.
func rawPlaces(raw []byte, kept []place) []json.RawMessage {
	var all struct {
		Places []json.RawMessage `json:"places"`
	}
	json.Unmarshal(raw, &all)
	ids := map[string]bool{}
	for _, p := range kept {
		ids[p.ID] = true
	}
	out := []json.RawMessage{}
	for _, r := range all.Places {
		var p struct{ ID string }
		if json.Unmarshal(r, &p) == nil && ids[p.ID] {
			out = append(out, r)
		}
	}
	return out
}

func (c *mapsClient) details(w io.Writer, id string, asJSON bool) error {
	var p place
	raw, err := c.call(http.MethodGet, "/places/"+url.PathEscape(id), mapsPlaceFields, nil, &p)
	if err != nil {
		return err
	}
	if asJSON {
		return writeIndentedJSON(w, raw)
	}
	json.Unmarshal(raw, &p.raw)

	fmt.Fprintln(w, p.DisplayName.Text)
	if line := ratingLine(&p); line != "" {
		fmt.Fprintln(w, line)
	}
	field := func(label, v string) {
		if v != "" {
			fmt.Fprintf(w, "%s: %s\n", label, v)
		}
	}
	field("Address", p.FormattedAddress)
	if p.Location != nil {
		field("Coordinates", fmt.Sprintf("%.6f, %.6f", p.Location.Latitude, p.Location.Longitude))
	}
	phone := p.NationalPhoneNumber
	if p.InternationalPhoneNumber != "" && phone != "" && p.InternationalPhoneNumber != phone {
		phone += " (" + p.InternationalPhoneNumber + ")"
	} else if phone == "" {
		phone = p.InternationalPhoneNumber
	}
	field("Phone", phone)
	field("Website", p.WebsiteURI)
	field("Google Maps", p.GoogleMapsURI)
	field("Directions", p.GoogleMapsLinks.DirectionsURI)
	field("id", p.ID)
	if loc := placeZone(&p); loc != nil {
		field("Local time there", c.now().In(loc).Format("Mon Jan 2, 3:04 PM"))
	}
	field("Now", c.status(&p))

	if h := p.CurrentOpeningHours; h != nil && len(h.WeekdayDescriptions) > 0 {
		fmt.Fprintln(w, "\nHours for the next 7 days (holiday changes included):")
		printLines(w, h.WeekdayDescriptions)
		if r := p.RegularOpeningHours; r != nil && len(r.WeekdayDescriptions) > 0 && !sameLines(r.WeekdayDescriptions, h.WeekdayDescriptions) {
			fmt.Fprintln(w, "Usual hours:")
			printLines(w, r.WeekdayDescriptions)
		}
	} else if r := p.RegularOpeningHours; r != nil && len(r.WeekdayDescriptions) > 0 {
		fmt.Fprintln(w, "\nUsual hours:")
		printLines(w, r.WeekdayDescriptions)
	} else {
		fmt.Fprintln(w, "\nNo opening hours listed.")
	}
	for _, h := range p.CurrentSecondaryOpeningHours {
		if len(h.WeekdayDescriptions) == 0 {
			continue
		}
		kind := strings.ReplaceAll(strings.ToLower(h.SecondaryHoursType), "_", " ")
		if kind == "" {
			kind = "other"
		}
		if !strings.HasSuffix(kind, "hour") {
			kind += " hours"
		}
		fmt.Fprintf(w, "%s:\n", strings.ToUpper(kind[:1])+kind[1:])
		printLines(w, h.WeekdayDescriptions)
	}

	fmt.Fprintln(w)
	field("About", p.EditorialSummary.Text)
	field("Overview (AI-written by Google)", p.GenerativeSummary.Overview.Text)
	field("What reviewers say (AI-written by Google)", p.ReviewSummary.Text.Text)
	var yes, no []string
	for _, f := range placeFeatures {
		var v bool
		if json.Unmarshal(p.raw[f.field], &v) != nil {
			continue
		}
		if v {
			yes = append(yes, f.label)
		} else {
			no = append(no, f.label)
		}
	}
	field("Has", strings.Join(yes, ", "))
	field("Doesn't have", strings.Join(no, ", "))
	field("Payment", options(p.PaymentOptions, "accepts"))
	field("Parking", options(p.ParkingOptions, ""))
	field("Wheelchair accessible", options(p.AccessibilityOptions, "wheelchairAccessible"))
	for _, f := range p.FuelOptions.FuelPrices {
		price := formatMoney(f.Price)
		if t, err := time.Parse(time.RFC3339, f.UpdateTime); err == nil {
			price += " (updated " + t.Format("Jan 2") + ")"
		}
		field("Fuel, "+humanizeEnum(f.Type), price)
	}

	if len(p.Reviews) > 0 {
		fmt.Fprintf(w, "\nReviews (Google's %d most relevant):\n", len(p.Reviews))
		for _, r := range p.Reviews {
			fmt.Fprintf(w, "- %s stars, %s, by %s:\n", strconv.FormatFloat(r.Rating, 'f', -1, 64), r.RelativeTime, r.AuthorAttribution.DisplayName)
			text := strings.TrimSpace(r.Text.Text)
			if text == "" {
				text = "(no text)"
			}
			for _, line := range strings.Split(text, "\n") {
				if line = strings.TrimSpace(line); line == "" {
					fmt.Fprintln(w)
				} else {
					fmt.Fprintf(w, "    %s\n", line)
				}
			}
		}
	}
	return nil
}

// ratingLine is "Pizza Restaurant · 4.6 stars (820 reviews) · $$ ($10–20)".
func ratingLine(p *place) string {
	var parts []string
	if t := p.PrimaryTypeDisplayName.Text; t != "" {
		parts = append(parts, t)
	}
	if p.UserRatingCount > 0 {
		parts = append(parts, fmt.Sprintf("%.1f stars (%s reviews)", p.Rating, commas(p.UserRatingCount)))
	}
	price := map[string]string{
		"PRICE_LEVEL_FREE":           "free",
		"PRICE_LEVEL_INEXPENSIVE":    "$",
		"PRICE_LEVEL_MODERATE":       "$$",
		"PRICE_LEVEL_EXPENSIVE":      "$$$",
		"PRICE_LEVEL_VERY_EXPENSIVE": "$$$$",
	}[p.PriceLevel]
	if r := p.PriceRange; r != nil && r.StartPrice != nil {
		rng := formatMoney(*r.StartPrice)
		if r.EndPrice != nil {
			rng += "–" + strings.TrimLeft(formatMoney(*r.EndPrice), "$")
		} else {
			rng += "+"
		}
		price = strings.TrimSpace(price + " (" + rng + " per person)")
	}
	if price != "" {
		parts = append(parts, price)
	}
	return strings.Join(parts, " · ")
}

// status is whether the place is open now and until when, in its local time.
func (c *mapsClient) status(p *place) string {
	switch p.BusinessStatus {
	case "CLOSED_PERMANENTLY":
		return "Permanently closed"
	case "CLOSED_TEMPORARILY":
		return "Temporarily closed"
	}
	h := p.CurrentOpeningHours
	if h == nil || h.OpenNow == nil {
		return "Hours unknown"
	}
	loc := placeZone(p)
	if loc == nil {
		loc = time.UTC
	}
	now := c.now().In(loc)
	if *h.OpenNow {
		if t, err := time.Parse(time.RFC3339, h.NextCloseTime); err == nil {
			return "Open now, closes " + when(t.In(loc), now)
		}
		return "Open now"
	}
	if t, err := time.Parse(time.RFC3339, h.NextOpenTime); err == nil {
		return "Closed now, opens " + when(t.In(loc), now)
	}
	return "Closed now"
}

// when formats t relative to now: "3:04 PM today", "midnight tonight",
// "Wed 3:04 PM" this week, "Wed Jan 2, 3:04 PM" after that.
func when(t, now time.Time) string {
	sameDay := func(a, b time.Time) bool {
		y1, m1, d1 := a.Date()
		y2, m2, d2 := b.Date()
		return y1 == y2 && m1 == m2 && d1 == d2
	}
	switch {
	case sameDay(t, now):
		return t.Format("3:04 PM") + " today"
	case t.Hour() == 0 && t.Minute() == 0 && sameDay(t, now.AddDate(0, 0, 1)):
		return "midnight tonight"
	case t.Sub(now) < 6*24*time.Hour:
		return t.Format("Mon 3:04 PM")
	default:
		return t.Format("Mon Jan 2, 3:04 PM")
	}
}

func placeZone(p *place) *time.Location {
	if p.UTCOffsetMinutes == nil {
		return nil
	}
	return time.FixedZone("", *p.UTCOffsetMinutes*60)
}

// options lists a set of yes/no options, the yes ones first:
// "credit cards, NFC (not: cash only)".
func options(m map[string]bool, prefix string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var yes, no []string
	for _, k := range keys {
		label := humanizeCamel(strings.TrimPrefix(k, prefix))
		if m[k] {
			yes = append(yes, label)
		} else {
			no = append(no, label)
		}
	}
	s := strings.Join(yes, ", ")
	if len(no) > 0 {
		if s == "" {
			s = "none"
		}
		s += " (not: " + strings.Join(no, ", ") + ")"
	}
	return s
}

// humanizeCamel turns "freeParkingLot" into "free parking lot" and "Nfc"
// into "NFC".
func humanizeCamel(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) && i > 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	out := strings.TrimSpace(b.String())
	return strings.ReplaceAll(out, "nfc", "NFC")
}

// humanizeEnum turns "REGULAR_UNLEADED" into "regular unleaded".
func humanizeEnum(s string) string {
	return strings.ReplaceAll(strings.ToLower(s), "_", " ")
}

func formatMoney(m money) string {
	units := m.Units
	if units == "" {
		units = "0"
	}
	amount := units
	if m.Nanos != 0 {
		n, _ := strconv.ParseFloat(units, 64)
		amount = strconv.FormatFloat(n+float64(m.Nanos)/1e9, 'f', 2, 64)
	}
	if m.CurrencyCode == "USD" || m.CurrencyCode == "" {
		return "$" + amount
	}
	return amount + " " + m.CurrencyCode
}

func commas(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// printLines prints Google's hour lines indented, with its thin and
// non-breaking spaces made plain.
func printLines(w io.Writer, lines []string) {
	for _, l := range lines {
		fmt.Fprintf(w, "  %s\n", plainSpaces(l))
	}
}

func plainSpaces(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\u202f' || r == '\u2009' || r == '\u00a0' {
			return ' '
		}
		return r
	}, s)
}

func sameLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func writeIndentedJSON(w io.Writer, raw []byte) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return err
	}
	buf.WriteByte('\n')
	_, err := buf.WriteTo(w)
	return err
}

func setMapsKey(key string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if key == "-" {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("reading the key from stdin: %w", err)
		}
		key = line
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("the key is empty")
	}

	// Try it first, so a typo isn't saved. If Google can't be reached from
	// here, save it anyway.
	cerr := checkMapsKey(key)
	var apiErr *mapsAPIError
	if errors.As(cerr, &apiErr) && (apiErr.HTTPStatus == http.StatusForbidden || apiErr.Reason == "API_KEY_INVALID") {
		return fmt.Errorf("not saved: Google rejected the key: %w", cerr)
	}
	if _, err := do(cfg, http.MethodPut, "/maps-key", strings.NewReader(key)); err != nil {
		return err
	}
	if cerr != nil {
		fmt.Printf("Key saved on the command center, but it couldn't be checked: %v\n", cerr)
	} else {
		fmt.Println("Key works with the Places API; saved on the command center.")
	}
	return nil
}

// checkMapsKey runs a search that asks only for place IDs, which Google
// doesn't bill.
func checkMapsKey(key string) error {
	c := &mapsClient{key: key, http: &http.Client{Timeout: 15 * time.Second}, now: time.Now}
	_, err := c.call(http.MethodPost, "/places:searchText", []string{"places.id"}, map[string]any{"textQuery": "coffee", "pageSize": 1}, nil)
	return err
}
