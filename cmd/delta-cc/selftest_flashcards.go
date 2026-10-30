package main

import "fmt"

// flashcardsSelfTest checks the sync (a new card is taken, a later change
// wins, an earlier one loses and is answered with what is kept, a removal
// stays) and pairing
// (a device's token reaches the flashcards and nothing else, until unpaired).
func flashcardsSelfTest(expect expectFunc, token string) error {
	sync := func(desc, tok string, c flashCollection) (flashCollection, error) {
		var out flashCollection
		_, err := expectJSON(expect, &out, desc, 200, "POST", "/flashcards/sync", tok, jsonBody(c))
		return out, err
	}
	deck := &flashDeck{ID: "spanish", Name: "Spanish", NewPerDay: -1, Mod: 1000}
	card := &flashCard{ID: "c1", Deck: "spanish", Front: "perro", Back: "dog", State: 1, Due: 5, Mod: 1000}
	got, err := sync("the first sync brings a deck", token, flashCollection{Zone: "America/Phoenix", Decks: []*flashDeck{deck}, Cards: []*flashCard{card}})
	if err != nil {
		return err
	}
	if got.USN != 1 || len(got.Decks) != 1 || len(got.Cards) != 1 || got.Zone != "America/Phoenix" {
		return fmt.Errorf("first sync: got %+v", got)
	}
	newer := *card
	newer.State, newer.Mod = 2, 2000
	if got, err = sync("a later review wins", token, flashCollection{USN: 1, Cards: []*flashCard{&newer}}); err != nil {
		return err
	}
	if got.USN != 2 || len(got.Cards) != 1 || got.Cards[0].State != 2 {
		return fmt.Errorf("later review: got %+v", got)
	}
	older := *card
	older.State, older.Mod = 3, 1500
	if got, err = sync("an earlier review loses", token, flashCollection{USN: 2, Cards: []*flashCard{&older}}); err != nil {
		return err
	}
	if got.USN != 2 || len(got.Cards) != 1 || got.Cards[0].State != 2 {
		return fmt.Errorf("earlier review: got %+v", got)
	}
	if got, err = sync("nothing new", token, flashCollection{USN: 2}); err != nil {
		return err
	}
	if len(got.Cards) != 0 || len(got.Decks) != 0 {
		return fmt.Errorf("nothing new: got %+v", got)
	}
	if _, err := expect("a card's text is corrected", 200, "PATCH", "/flashcards/cards/c1", token, []byte(`{"front":"el perro"}`)); err != nil {
		return err
	}
	stale := newer
	stale.Front, stale.State, stale.Mod = "perro", 1, 3000
	if got, err = sync("a device that hasn't heard of the edit answers the card", token, flashCollection{USN: 2, Cards: []*flashCard{&stale}}); err != nil {
		return err
	}
	if len(got.Cards) != 1 || got.Cards[0].Front != "el perro" || got.Cards[0].State != 1 {
		return fmt.Errorf("the answer is taken but not the old text: got %+v", got.Cards)
	}
	gone := *got.Cards[0]
	gone.Deleted, gone.Mod = true, 2500
	if got, err = sync("a device removes the card, though another answered it later", token, flashCollection{USN: got.USN, Cards: []*flashCard{&gone}}); err != nil {
		return err
	}
	if len(got.Cards) != 1 || !got.Cards[0].Deleted {
		return fmt.Errorf("the removal is taken: got %+v", got.Cards)
	}
	answered := stale
	answered.Mod = 9000
	if got, err = sync("a device that hasn't heard of the removal answers the card", token, flashCollection{USN: got.USN, Cards: []*flashCard{&answered}}); err != nil {
		return err
	}
	if len(got.Cards) != 1 || !got.Cards[0].Deleted {
		return fmt.Errorf("the card stays removed: got %+v", got.Cards)
	}
	if err := runSteps(expect,
		step{"a removed card isn't corrected", 404, "PATCH", "/flashcards/cards/c1", token, []byte(`{"front":"perro"}`)},
		step{"a bad card is refused", 400, "POST", "/flashcards/sync", token, []byte(`{"cards":[{"id":"x","deck":"d","state":9}]}`)},
	); err != nil {
		return err
	}

	var pair struct{ Code, Secret string }
	if _, err := expectJSON(expect, &pair, "a device starts pairing", 201, "POST", "/flashcards/pair", "", []byte(`{"name":"reMarkable"}`)); err != nil {
		return err
	}
	if err := runSteps(expect,
		step{"pairing waits", 202, "GET", "/flashcards/pair/" + pair.Secret, "", nil},
		step{"a wrong code is refused", 404, "POST", "/flashcards/pair/approve", token, []byte(`{"code":"AAA-AAA"}`)},
		step{"approving needs the delta token", 401, "POST", "/flashcards/pair/approve", "", []byte(`{"code":"` + pair.Code + `"}`)},
	); err != nil {
		return err
	}
	var device flashDevice
	if _, err := expectJSON(expect, &device, "the website approves", 201, "POST", "/flashcards/pair/approve", token, []byte(`{"code":"`+pair.Code+`"}`)); err != nil {
		return err
	}
	if device.Name != "reMarkable" {
		return fmt.Errorf("the device is called %q, not what it called itself", device.Name)
	}
	var paired struct{ Token string }
	if _, err := expectJSON(expect, &paired, "the device gets its token", 200, "GET", "/flashcards/pair/"+pair.Secret, "", nil); err != nil {
		return err
	}
	if _, err := expect("only once", 404, "GET", "/flashcards/pair/"+pair.Secret, "", nil); err != nil {
		return err
	}
	if _, err := sync("the device syncs", paired.Token, flashCollection{USN: 2}); err != nil {
		return err
	}
	return runSteps(expect,
		step{"the device reads the collection", 200, "GET", "/flashcards", paired.Token, nil},
		step{"the device reaches nothing else", 403, "GET", "/files", paired.Token, nil},
		step{"nor pairs others", 403, "POST", "/flashcards/pair/approve", paired.Token, []byte(`{"code":"AAA-AAA"}`)},
		step{"unpairing", 200, "DELETE", "/flashcards/devices/" + device.ID, token, nil},
		step{"an unpaired device is refused", 401, "POST", "/flashcards/sync", paired.Token, []byte(`{}`)},
	)
}
