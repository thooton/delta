package main

import (
	"encoding/json"
	"fmt"
)

// flashcardsSelfTest checks the sync (a new card is taken, a later change
// wins, an earlier one loses and is answered with what is kept) and pairing
// (a device's token reaches the flashcards and nothing else, until unpaired).
func flashcardsSelfTest(expect func(desc string, want int, method, path, tok string, body []byte) ([]byte, error), token string) error {
	type collection struct {
		USN   int64        `json:"usn"`
		Zone  string       `json:"zone,omitempty"`
		Decks []*flashDeck `json:"decks"`
		Cards []*flashCard `json:"cards"`
	}
	sync := func(desc, tok string, c collection) (collection, error) {
		body, _ := json.Marshal(c)
		data, err := expect(desc, 200, "POST", "/flashcards/sync", tok, body)
		var out collection
		if err == nil {
			err = json.Unmarshal(data, &out)
		}
		return out, err
	}
	deck := &flashDeck{ID: "spanish", Name: "Spanish", NewPerDay: -1, Mod: 1000}
	card := &flashCard{ID: "c1", Deck: "spanish", Front: "perro", Back: "dog", State: 1, Due: 5, Mod: 1000}
	got, err := sync("the first sync brings a deck", token, collection{Zone: "America/Phoenix", Decks: []*flashDeck{deck}, Cards: []*flashCard{card}})
	if err != nil {
		return err
	}
	if got.USN != 1 || len(got.Decks) != 1 || len(got.Cards) != 1 || got.Zone != "America/Phoenix" {
		return fmt.Errorf("first sync: got %+v", got)
	}
	newer := *card
	newer.State, newer.Mod = 2, 2000
	if got, err = sync("a later review wins", token, collection{USN: 1, Cards: []*flashCard{&newer}}); err != nil {
		return err
	}
	if got.USN != 2 || len(got.Cards) != 1 || got.Cards[0].State != 2 {
		return fmt.Errorf("later review: got %+v", got)
	}
	older := *card
	older.State, older.Mod = 3, 1500
	if got, err = sync("an earlier review loses", token, collection{USN: 2, Cards: []*flashCard{&older}}); err != nil {
		return err
	}
	if got.USN != 2 || len(got.Cards) != 1 || got.Cards[0].State != 2 {
		return fmt.Errorf("earlier review: got %+v", got)
	}
	if got, err = sync("nothing new", token, collection{USN: 2}); err != nil {
		return err
	}
	if len(got.Cards) != 0 || len(got.Decks) != 0 {
		return fmt.Errorf("nothing new: got %+v", got)
	}
	if _, err := expect("a bad card is refused", 400, "POST", "/flashcards/sync", token, []byte(`{"cards":[{"id":"x","deck":"d","state":9}]}`)); err != nil {
		return err
	}

	data, err := expect("a device starts pairing", 201, "POST", "/flashcards/pair", "", []byte(`{"name":"reMarkable"}`))
	if err != nil {
		return err
	}
	var pair struct{ Code, Secret string }
	json.Unmarshal(data, &pair)
	if _, err := expect("pairing waits", 202, "GET", "/flashcards/pair/"+pair.Secret, "", nil); err != nil {
		return err
	}
	if _, err := expect("a wrong code is refused", 404, "POST", "/flashcards/pair/approve", token, []byte(`{"code":"AAA-AAA"}`)); err != nil {
		return err
	}
	if _, err := expect("approving needs the delta token", 401, "POST", "/flashcards/pair/approve", "", []byte(`{"code":"`+pair.Code+`"}`)); err != nil {
		return err
	}
	data, err = expect("the website approves", 201, "POST", "/flashcards/pair/approve", token, []byte(`{"code":"`+pair.Code+`"}`))
	if err != nil {
		return err
	}
	var device flashDevice
	json.Unmarshal(data, &device)
	if device.Name != "reMarkable" {
		return fmt.Errorf("the device is called %q, not what it called itself", device.Name)
	}
	data, err = expect("the device gets its token", 200, "GET", "/flashcards/pair/"+pair.Secret, "", nil)
	if err != nil {
		return err
	}
	var paired struct{ Token string }
	json.Unmarshal(data, &paired)
	if _, err := expect("only once", 404, "GET", "/flashcards/pair/"+pair.Secret, "", nil); err != nil {
		return err
	}
	if _, err := sync("the device syncs", paired.Token, collection{USN: 2}); err != nil {
		return err
	}
	if _, err := expect("the device reads the collection", 200, "GET", "/flashcards", paired.Token, nil); err != nil {
		return err
	}
	if _, err := expect("the device reaches nothing else", 403, "GET", "/files", paired.Token, nil); err != nil {
		return err
	}
	if _, err := expect("nor pairs others", 403, "POST", "/flashcards/pair/approve", paired.Token, []byte(`{"code":"AAA-AAA"}`)); err != nil {
		return err
	}
	if _, err := expect("unpairing", 200, "DELETE", "/flashcards/devices/"+device.ID, token, nil); err != nil {
		return err
	}
	_, err = expect("an unpaired device is refused", 401, "POST", "/flashcards/sync", paired.Token, []byte(`{}`))
	return err
}
