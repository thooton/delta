package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseDistance(t *testing.T) {
	cases := map[string]float64{
		"2mi": 2 * metersPerMile, "1.5 km": 1500, "500m": 500, "800ft": 243.84,
		"3": 3 * metersPerMile, ".5 miles": 0.5 * metersPerMile, "10 meters": 10,
	}
	for in, want := range cases {
		got, err := parseDistance(in)
		if err != nil || math.Abs(got-want) > 0.01 {
			t.Errorf("parseDistance(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "far", "0", "2 yards", "-1mi"} {
		if _, err := parseDistance(bad); err == nil {
			t.Errorf("parseDistance(%q) succeeded", bad)
		}
	}
}

func TestPlaceID(t *testing.T) {
	for _, id := range []string{"ChIJ9afkmet5bIcRp8sdvzfnlHM", "places/ChIJg87rJbN5bIcRS2HgCCwJ108", "GhIJQWDl0CIeQUARxks3icF8U8A"} {
		if _, ok := placeID(id); !ok {
			t.Errorf("placeID(%q) = false", id)
		}
	}
	for _, name := range []string{"Starbucks", "Coors Field, Denver", "Union Station Denver CO", "pizza"} {
		if _, ok := placeID(name); ok {
			t.Errorf("placeID(%q) = true", name)
		}
	}
}

func TestParseLatLng(t *testing.T) {
	if ll, ok := parseLatLng(" 39.75, -104.99 "); !ok || ll != (latLng{39.75, -104.99}) {
		t.Errorf("parseLatLng = %v, %v", ll, ok)
	}
	for _, bad := range []string{"85281", "Denver, CO", "91,0", "0,181"} {
		if _, ok := parseLatLng(bad); ok {
			t.Errorf("parseLatLng(%q) succeeded", bad)
		}
	}
}

func TestBoundingBox(t *testing.T) {
	b := boundingBox(latLng{0, 179.99}, 10000)
	if b["low"].Longitude < 179 || b["high"].Longitude > -179 {
		t.Errorf("box across the antimeridian = %v", b)
	}
	c := latLng{39.75, -104.99}
	b = boundingBox(c, 1000)
	for _, corner := range []latLng{{b["low"].Latitude, c.Longitude}, {c.Latitude, b["high"].Longitude}} {
		if d := distance(c, corner); math.Abs(d-1000) > 5 {
			t.Errorf("box edge is %.0f m from the center, want 1000", d)
		}
	}
}

func TestWhen(t *testing.T) {
	loc := time.FixedZone("", -6*3600)
	now := time.Date(2026, 10, 13, 21, 0, 0, 0, loc) // a Tuesday
	cases := []struct {
		t    time.Time
		want string
	}{
		{time.Date(2026, 10, 13, 23, 0, 0, 0, loc), "11:00 PM today"},
		{time.Date(2026, 10, 14, 0, 0, 0, 0, loc), "midnight tonight"},
		{time.Date(2026, 10, 14, 2, 15, 0, 0, loc), "Wed 2:15 AM"},
		{time.Date(2026, 10, 25, 9, 0, 0, 0, loc), "Sun Oct 25, 9:00 AM"},
	}
	for _, c := range cases {
		if got := when(c.t, now); got != c.want {
			t.Errorf("when(%v) = %q, want %q", c.t, got, c.want)
		}
	}
}

// fakePlaces serves canned Places API responses and records the requests.
func fakePlaces(t *testing.T, handle func(r *http.Request, body map[string]any) any) *mapsClient {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Goog-Api-Key") != "test-key" || r.Header.Get("X-Goog-FieldMask") == "" {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":{"code":403,"message":"bad key","status":"PERMISSION_DENIED"}}`))
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(handle(r, body))
	}))
	t.Cleanup(ts.Close)
	old := mapsAPI
	mapsAPI = ts.URL
	t.Cleanup(func() { mapsAPI = old })
	now := time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC) // 9 PM in Denver
	return &mapsClient{key: "test-key", http: ts.Client(), now: func() time.Time { return now }}
}

func TestMapsSearchNear(t *testing.T) {
	var searched map[string]any
	c := fakePlaces(t, func(r *http.Request, body map[string]any) any {
		if body["textQuery"] == "Union Station, Denver" {
			return map[string]any{"places": []any{map[string]any{
				"id": "anchor", "displayName": map[string]any{"text": "Union Station"},
				"formattedAddress": "Union Station, Denver, CO", "location": map[string]any{"latitude": 39.7527, "longitude": -104.9994},
			}}}
		}
		searched = body
		return map[string]any{"places": []any{
			map[string]any{
				"id": "near", "displayName": map[string]any{"text": "Close Pizza"},
				"formattedAddress": "1 Near St", "location": map[string]any{"latitude": 39.7530, "longitude": -104.9990},
				"rating": 4.5, "userRatingCount": 2691, "priceLevel": "PRICE_LEVEL_INEXPENSIVE", "utcOffsetMinutes": -360,
				"currentOpeningHours": map[string]any{"openNow": true, "nextCloseTime": "2026-10-14T05:00:00Z"},
			},
			map[string]any{
				// Inside the box, outside the circle.
				"id": "corner", "displayName": map[string]any{"text": "Corner Pizza"},
				"location": map[string]any{"latitude": 39.7527 + 0.0089, "longitude": -104.9994 + 0.0115},
			},
		}}
	})
	var out strings.Builder
	err := c.search(&out, mapsSearch{Query: "pizza", Near: "Union Station, Denver", Within: 1000, Closest: true, OpenNow: true, Limit: 5}, false)
	if err != nil {
		t.Fatal(err)
	}
	if searched["rankPreference"] != "DISTANCE" || searched["openNow"] != true || searched["locationRestriction"] == nil || searched["locationBias"] != nil {
		t.Errorf("search request = %v", searched)
	}
	got := out.String()
	for _, want := range []string{
		"Near: Union Station, Denver, CO (within 0.6 mi)",
		"Local time there: Tue Oct 13, 9:00 PM",
		"1. Close Pizza\n   4.5 stars (2,691 reviews) · $\n   1 Near St · 157 ft away\n   Open now, closes 11:00 PM today\n   id: near",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Corner Pizza") {
		t.Errorf("a place outside --within was kept:\n%s", got)
	}
}

func TestMapsDetails(t *testing.T) {
	c := fakePlaces(t, func(r *http.Request, body map[string]any) any {
		if r.URL.Path != "/places/ChIJ9afkmet5bIcRp8sdvzfnlHM" {
			t.Errorf("details path = %s", r.URL.Path)
		}
		return map[string]any{
			"id": "ChIJ9afkmet5bIcRp8sdvzfnlHM", "displayName": map[string]any{"text": "Mario's"},
			"primaryTypeDisplayName": map[string]any{"text": "Pizza Restaurant"},
			"priceRange":             map[string]any{"startPrice": map[string]any{"currencyCode": "USD", "units": "10"}, "endPrice": map[string]any{"currencyCode": "USD", "units": "20"}},
			"utcOffsetMinutes":       -360, "businessStatus": "OPERATIONAL",
			"currentOpeningHours": map[string]any{
				"openNow": false, "nextOpenTime": "2026-10-14T22:00:00Z",
				"weekdayDescriptions": []string{"Monday: Closed", "Tuesday: 4:00\u202fPM\u2009–\u20092:15\u202fAM"},
			},
			"currentSecondaryOpeningHours": []any{map[string]any{"secondaryHoursType": "HAPPY_HOUR", "weekdayDescriptions": []string{"Tuesday: 4:00\u2009–\u20098:00\u202fPM"}}},
			"dineIn":                       true, "curbsidePickup": false,
			"paymentOptions": map[string]bool{"acceptsNfc": true, "acceptsCashOnly": false},
			"reviews": []any{map[string]any{
				"rating": 5, "relativePublishTimeDescription": "4 months ago",
				"text": map[string]any{"text": "Great.\n\nWould go again."}, "authorAttribution": map[string]any{"displayName": "Jeff"},
			}},
		}
	})
	var out strings.Builder
	if err := c.details(&out, "ChIJ9afkmet5bIcRp8sdvzfnlHM", false); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"Pizza Restaurant · ($10–20 per person)",
		"Now: Closed now, opens Wed 4:00 PM",
		"  Tuesday: 4:00 PM – 2:15 AM\n",
		"Happy hour:\n  Tuesday: 4:00 – 8:00 PM\n",
		"Has: dine-in\nDoesn't have: curbside pickup\n",
		"Payment: NFC (not: cash only)\n",
		"- 5 stars, 4 months ago, by Jeff:\n    Great.\n\n    Would go again.\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestMapsAPIError(t *testing.T) {
	c := fakePlaces(t, nil)
	c.key = "wrong"
	_, err := c.call(http.MethodPost, "/places:searchText", []string{"places.id"}, map[string]any{"textQuery": "x"}, nil)
	apiErr, ok := err.(*mapsAPIError)
	if !ok || apiErr.HTTPStatus != 403 || err.Error() != "Google Maps: bad key (PERMISSION_DENIED)" {
		t.Errorf("err = %v", err)
	}
}
