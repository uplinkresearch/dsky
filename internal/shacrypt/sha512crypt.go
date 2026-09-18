// Package shacrypt makes the password hash a Linux installer will accept in
// its answers, so a stick can install without stopping to ask for an account.
//
// This is SHA-512 crypt, the "$6$" scheme, as specified by Ulrich Drepper. It
// is what `openssl passwd -6` and `mkpasswd -m sha-512` produce, what
// subiquity's `identity.password` and Anaconda's `--iscrypted` expect, and
// what /etc/shadow holds afterwards.
//
// Written out here rather than shelled out to, because DSKY builds Linux media
// from Windows and macOS too, where there is no openssl to call. The algorithm
// is fixed and fully specified, and the tests below check it against the
// published vectors rather than against itself.
package shacrypt

import (
	"crypto/rand"
	"crypto/sha512"
	"fmt"
	"strings"
)

// saltAlphabet is what the scheme allows in a salt, and what the encoding
// below emits: the same 64 characters, in crypt's own order, which is not
// base64's.
const saltAlphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// defaultRounds is the scheme's own default, and is left out of the hash when
// used -- a "$6$rounds=5000$" prefix would be unusual enough to look like a
// setting somebody chose.
const defaultRounds = 5000

// Salt makes a random 16-character salt, the maximum the scheme allows.
func Salt() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("shacrypt: no randomness for a salt: %w", err)
	}
	out := make([]byte, len(b))
	for i, v := range b {
		out[i] = saltAlphabet[int(v)%len(saltAlphabet)]
	}
	return string(out), nil
}

// Hash returns the $6$ hash of password with salt, ready to be written into an
// installer's answers.
func Hash(password, salt string) string {
	return hashRounds(password, salt, defaultRounds)
}

func hashRounds(password, salt string, rounds int) string {
	pw := []byte(password)
	sa := []byte(salt)
	if len(sa) > 16 {
		sa = sa[:16]
	}

	// B: the hash of password + salt + password.
	b := sha512.Sum512(concat(pw, sa, pw))

	// A: password + salt, then B repeated for the length of the password, then
	// one bit of B or of the password for each bit of the password's length.
	var a []byte
	a = append(a, pw...)
	a = append(a, sa...)
	for i := 0; i < len(pw); i += sha512.Size {
		a = append(a, b[:min(sha512.Size, len(pw)-i)]...)
	}
	for i := len(pw); i > 0; i >>= 1 {
		if i&1 != 0 {
			a = append(a, b[:]...)
		} else {
			a = append(a, pw...)
		}
	}
	sum := sha512.Sum512(a)

	// DP: the password repeated, hashed; P: that stretched to the password's
	// length. Likewise DS/S for the salt.
	var dp []byte
	for i := 0; i < len(pw); i++ {
		dp = append(dp, pw...)
	}
	dpSum := sha512.Sum512(dp)
	p := stretch(dpSum[:], len(pw))

	var ds []byte
	for i := 0; i < 16+int(sum[0]); i++ {
		ds = append(ds, sa...)
	}
	dsSum := sha512.Sum512(ds)
	s := stretch(dsSum[:], len(sa))

	// The rounds: the whole point, and the reason this is slow on purpose.
	cur := sum
	for i := 0; i < rounds; i++ {
		var c []byte
		if i&1 != 0 {
			c = append(c, p...)
		} else {
			c = append(c, cur[:]...)
		}
		if i%3 != 0 {
			c = append(c, s...)
		}
		if i%7 != 0 {
			c = append(c, p...)
		}
		if i&1 != 0 {
			c = append(c, cur[:]...)
		} else {
			c = append(c, p...)
		}
		cur = sha512.Sum512(c)
	}

	var b64 strings.Builder
	// The scheme's own byte order, which is neither the digest's nor any
	// obvious rotation of it; it is simply the order the specification gives.
	order := [][3]int{
		{0, 21, 42}, {22, 43, 1}, {44, 2, 23}, {3, 24, 45}, {25, 46, 4}, {47, 5, 26},
		{6, 27, 48}, {28, 49, 7}, {50, 8, 29}, {9, 30, 51}, {31, 52, 10}, {53, 11, 32},
		{12, 33, 54}, {34, 55, 13}, {56, 14, 35}, {15, 36, 57}, {37, 58, 16}, {59, 17, 38},
		{18, 39, 60}, {40, 61, 19}, {62, 20, 41},
	}
	for _, t := range order {
		encode(&b64, cur[t[0]], cur[t[1]], cur[t[2]], 4)
	}
	encode(&b64, 0, 0, cur[63], 2)

	if rounds == defaultRounds {
		return "$6$" + string(sa) + "$" + b64.String()
	}
	return fmt.Sprintf("$6$rounds=%d$%s$%s", rounds, sa, b64.String())
}

// encode writes n characters of crypt's little-endian base64 for three bytes.
func encode(b *strings.Builder, c2, c1, c0 byte, n int) {
	w := uint(c2)<<16 | uint(c1)<<8 | uint(c0)
	for i := 0; i < n; i++ {
		b.WriteByte(saltAlphabet[w&0x3f])
		w >>= 6
	}
}

// stretch repeats a digest until it is n bytes long, as the scheme's P and S
// sequences are built.
func stretch(sum []byte, n int) []byte {
	out := make([]byte, 0, n)
	for len(out) < n {
		out = append(out, sum[:min(sha512.Size, n-len(out))]...)
	}
	return out
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// HashPassword salts and hashes a password for an installer's answers, and
// refuses an empty one: the whole point is an account somebody can sign in to
// without the installer stopping to ask, and an account with no password is
// not that -- it is an open machine, made by a tool that was asked for the
// opposite.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", fmt.Errorf("shacrypt: refusing to hash an empty password")
	}
	salt, err := Salt()
	if err != nil {
		return "", err
	}
	return Hash(password, salt), nil
}
