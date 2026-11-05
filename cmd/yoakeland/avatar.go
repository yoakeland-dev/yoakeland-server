package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// A person's picture is stored once in the upload bucket and named by its content hash, as
// "upload:<sha256>", instead of travelling as a data URL on every message and every event.
// An emoji is its own value and never leaves the user record.

const avatarPrefix = "upload:"

// avatarFile is the name a stored picture has inside its hash's directory.
const avatarFile = "avatar"

var avatarTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// avatarHash is the bucket hash an avatar value names, or "" for an emoji or anything else.
func avatarHash(a string) string {
	if h, ok := strings.CutPrefix(a, avatarPrefix); ok && sha256Hex.MatchString(h) {
		return h
	}
	return ""
}

// decodeDataURL splits a base64 image data URL into its type and bytes.
func decodeDataURL(a string) (mime string, b []byte, ok bool) {
	rest, found := strings.CutPrefix(a, "data:")
	if !found {
		return "", nil, false
	}
	head, body, found := strings.Cut(rest, ",")
	mime, isB64 := strings.CutSuffix(head, ";base64")
	if !found || !isB64 || !avatarTypes[mime] {
		return "", nil, false
	}
	b, err := base64.StdEncoding.DecodeString(body)
	return mime, b, err == nil && len(b) > 0
}

// avatarKey says which picture an avatar is, whatever form it is stored in: the bucket hash for
// "upload:<sha>" and for an old data URL (the same bytes give the same hash), the emoji itself
// otherwise. Two avatars with the same key are the same picture.
func avatarKey(a string) string {
	if h := avatarHash(a); h != "" {
		return h
	}
	if _, b, ok := decodeDataURL(a); ok {
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:])
	}
	return a
}

// PutAvatar stores the picture a data URL holds and returns the avatar value that names it. A
// value that isn't an image data URL (an emoji, or already "upload:...") comes back as it is.
func (s *UploadStore) PutAvatar(a string) (string, error) {
	_, b, ok := decodeDataURL(a)
	if !ok {
		if strings.HasPrefix(a, "data:") {
			return "", fmt.Errorf("avatar must be a png, jpeg, gif or webp image")
		}
		return a, nil
	}
	if s == nil {
		return a, nil
	}
	sum := sha256.Sum256(b)
	sha := hex.EncodeToString(sum[:])
	dir := filepath.Join(s.dir, sha)
	path := filepath.Join(dir, avatarFile)
	if _, err := os.Stat(path); err != nil {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
		tmp, err := os.CreateTemp(filepath.Join(s.dir, ".tmp"), "avatar-*")
		if err != nil {
			return "", err
		}
		_, werr := tmp.Write(b)
		if cerr := tmp.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			_ = os.Remove(tmp.Name())
			return "", werr
		}
		_ = os.Chmod(tmp.Name(), 0o600)
		if err := os.Rename(tmp.Name(), path); err != nil {
			_ = os.Remove(tmp.Name())
			return "", err
		}
	}
	s.Touch(path)
	return avatarPrefix + sha, nil
}

// AvatarDataURL returns the picture an "upload:<sha>" avatar names as a data URL.
func (s *UploadStore) AvatarDataURL(a string) (string, bool) {
	sha := avatarHash(a)
	if s == nil || sha == "" {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(s.dir, sha, avatarFile))
	if err != nil || len(b) == 0 {
		return "", false
	}
	mime := http.DetectContentType(b)
	if !avatarTypes[mime] {
		return "", false
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b), true
}
