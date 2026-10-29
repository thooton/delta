package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// writePrivateFile replaces path with data, readable only by the command
// center's user. It writes a temporary file and renames it into place, so
// readers never see a partial file.
func writePrivateFile(path string, data []byte) error {
	return writeFileAtomic(path, data, 0o600)
}

// writeFileAtomic is writePrivateFile with the file's mode given.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && perm != 0o600 {
		err = os.Chmod(tmp.Name(), perm)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	return err
}

// writeJSONFile replaces path with v as indented JSON, readable only by the
// command center's user.
func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(path, append(data, '\n'))
}

// randomHex returns n random bytes in hex.
func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// keyHash is how keys and tokens are kept: the SHA-256 of the key, in hex.
func keyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// randomToken returns n random bytes in unpadded URL-safe Base64.
func randomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// writeKeyHash keeps the hash of a newly made secret in name in dataDir, for
// the gen-token and shorthand-secret commands, and returns the file's path.
func writeKeyHash(dataDir, name, secret string) (string, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dataDir, name)
	return path, writePrivateFile(path, []byte(keyHash(secret)+"\n"))
}

// randomUUID returns a random (version 4) UUID.
func randomUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// readJSONList reads a file holding a JSON list, which is empty while the
// file is missing.
func readJSONList[T any](path string) ([]T, error) {
	list := []T{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return list, nil
	} else if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("malformed %s: %v", filepath.Base(path), err)
	}
	if list == nil {
		list = []T{}
	}
	return list, nil
}
