package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonMemory  = 64 * 1024 // KiB
	argonTime    = 3
	argonThreads = 2
	argonSaltLen = 16
	argonKeyLen  = 32
)

var b64 = base64.RawStdEncoding

// Each Argon2id run holds 64 MiB, so at most one per CPU runs at a time.
var argonSem = make(chan struct{}, runtime.NumCPU())

// argonHook, when set by a test, is told +1/-1 as a run starts and ends.
var argonHook func(delta int)

func idKey(ctx context.Context, pw, salt []byte, t, m uint32, p uint8, keyLen uint32) ([]byte, error) {
	select {
	case argonSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-argonSem }()
	if argonHook != nil {
		argonHook(1)
		defer argonHook(-1)
	}
	return argon2.IDKey(pw, salt, t, m, p, keyLen), nil
}

// HashPassword returns an Argon2id PHC string:
// $argon2id$v=19$m=65536,t=3,p=2$<salt>$<key>.
func HashPassword(pw string) (string, error) { return hashPassword(context.Background(), pw) }

func hashPassword(ctx context.Context, pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := idKey(ctx, []byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// CheckPassword reports whether pw matches hash, using the parameters stored in the hash.
func CheckPassword(hash, pw string) (bool, error) {
	return checkPassword(context.Background(), hash, pw)
}

func checkPassword(ctx context.Context, hash, pw string) (bool, error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return false, errors.New("auth: not an argon2id v19 hash")
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil || t == 0 || p == 0 {
		return false, errors.New("auth: bad argon2 parameters")
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, errors.New("auth: bad argon2 key")
	}
	got, err := idKey(ctx, []byte(pw), salt, t, m, p, uint32(len(want)))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
