// Package fkv reads FKV (Flashcards Key-Value), the flashcard format
// Transform makes and Flashcards on the phone imports.
//
// An FKV file is UTF-8 text, one card per line: key||value, or
// key||value||metadata, where the metadata is optional extra text shown with
// the answer (an example sentence, say). Each part is written as the body of
// a string literal: a backslash starts an
// escape (\n, \r, \t, \\, \|, \0, \a, \b, \f, \v, \', \", \xHH, \uHHHH,
// \UHHHHHHHH), and anything else stands for itself, quotes included. Each
// unescaped "||" ends a part; a third one in the line is an error, so a
// literal || is written \|\| (or \||). Blank lines are skipped,
// a \r before a line's end is dropped (CRLF files), and so is a leading BOM.
// A card needs a key and a value; the metadata may be empty.
//
// The phone's parser (Fkv in sonim's flashcards core) follows the same rules;
// change them together.
package fkv

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Card is one card: its front, its back, and its metadata ("" for none).
type Card struct {
	Key, Value, Meta string
}

// Parse reads an FKV file, or says what is wrong with it, naming the line.
func Parse(data []byte) ([]Card, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("an FKV file must be UTF-8")
	}
	text := strings.TrimPrefix(string(data), "\uFEFF")
	var cards []Card
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		card, err := parseLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", i+1, err)
		}
		cards = append(cards, card)
	}
	return cards, nil
}

// parseLine reads one line, key||value or key||value||metadata.
func parseLine(line string) (Card, error) {
	var parts [3]strings.Builder
	part := 0
	for i := 0; i < len(line); {
		switch c := line[i]; {
		case c == '\\':
			r, n, err := unescape(line[i:])
			if err != nil {
				return Card{}, err
			}
			parts[part].WriteRune(r)
			i += n
		case c == '|' && i+1 < len(line) && line[i+1] == '|':
			if part == 2 {
				return Card{}, errors.New(`a third || (write a literal one as \|\|)`)
			}
			part++
			i += 2
		default:
			parts[part].WriteByte(c)
			i++
		}
	}
	switch {
	case part == 0:
		return Card{}, errors.New("no || between the key and the value")
	case parts[0].Len() == 0:
		return Card{}, errors.New("the key is empty")
	case parts[1].Len() == 0:
		return Card{}, errors.New("the value is empty")
	}
	return Card{parts[0].String(), parts[1].String(), parts[2].String()}, nil
}

var simple = map[byte]rune{
	'n': '\n', 'r': '\r', 't': '\t', '\\': '\\', '|': '|', '0': 0,
	'a': '\a', 'b': '\b', 'f': '\f', 'v': '\v', '\'': '\'', '"': '"',
}

// unescape reads the escape s starts with: the character, and its length.
func unescape(s string) (rune, int, error) {
	if len(s) < 2 {
		return 0, 0, errors.New(`a lone \ at the end of the line (write a literal one as \\)`)
	}
	if r, ok := simple[s[1]]; ok {
		return r, 2, nil
	}
	digits := map[byte]int{'x': 2, 'u': 4, 'U': 8}[s[1]]
	if digits == 0 {
		r, _ := utf8.DecodeRuneInString(s[1:])
		return 0, 0, fmt.Errorf(`\%c isn't an escape (write a literal backslash as \\)`, r)
	}
	if len(s) < 2+digits {
		return 0, 0, fmt.Errorf(`\%c needs %d hex digits`, s[1], digits)
	}
	n, err := strconv.ParseUint(s[2:2+digits], 16, 32)
	if err != nil || n > utf8.MaxRune || (n >= 0xD800 && n <= 0xDFFF) {
		return 0, 0, fmt.Errorf(`%s isn't a character`, s[:2+digits])
	}
	return rune(n), 2 + digits, nil
}
