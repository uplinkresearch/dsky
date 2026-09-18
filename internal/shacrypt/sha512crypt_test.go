package shacrypt

import (
	"os/exec"
	"strings"
	"testing"
)

// The vectors from the specification itself. A hash function tested against
// its own output proves only that it is consistent, which a wrong one also is.
func TestPublishedVectors(t *testing.T) {
	cases := []struct {
		rounds   int
		salt, pw string
		want     string
	}{
		{5000, "saltstring", "Hello world!",
			"$6$saltstring$svn8UoSVapNtMuq1ukKS4tPQd8iKwSMHWjl/O817G3uBnIFNjnQJuesI68u4OTLiBFdcbYEdFCoEOfaS35inz1"},
		{10000, "saltstringsaltstring", "Hello world!",
			"$6$rounds=10000$saltstringsaltst$OW1/O6BYHV6BcXZu8QVeXbDWra3Oeqh0sbHbbMCVNSnCM/UrjmM0Dp8vOuZeHBy/YTBmSK6H9qs/y3RnOaw5v."},
		{5000, "toolongsaltstring", "This is just a test",
			"$6$toolongsaltstrin$lQ8jolhgVRVhY4b5pZKaysCLi0QBxGoNeKQzQ3glMhwllF7oGDZxUhx1yxdYcz/e1JSbq3y6JMxxl8audkUEm0"},
	}
	// No empty-password vector: the specification publishes none, openssl
	// refuses to hash one at all, and writing down whatever this code happens
	// to produce would pin the implementation to itself rather than to the
	// scheme. Callers must refuse an empty password anyway -- see HashPassword.
	for _, c := range cases {
		got := hashRounds(c.pw, c.salt, c.rounds)
		if got != c.want {
			t.Errorf("rounds=%d salt=%q pw=%q\n got %s\nwant %s", c.rounds, c.salt, c.pw, got, c.want)
		}
	}
}

// And against whatever openssl this machine has, when there is one to ask.
// The vectors above are the real check; this catches a vector transcribed
// wrongly, which would otherwise pin the same mistake twice.
//
// ASCII only, deliberately. A non-ASCII password was in this list and passed
// on Linux and failed on Windows, because passing one through a shell to
// another program compares how the two encode an argument rather than how
// they hash it -- the Go side hashes the UTF-8 bytes it holds, and whatever
// PowerShell handed openssl was not those bytes. The scheme hashes bytes and
// has no opinion about text, so there is nothing about Unicode for this
// particular oracle to settle.
func TestAgreesWithOpenSSL(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("no openssl to compare against")
	}
	for _, pw := range []string{"hunter2", "a", "correct horse battery staple", "p@ss w/ spaces!"} {
		out, err := exec.Command("openssl", "passwd", "-6", "-salt", "dskysalt", pw).Output()
		if err != nil {
			t.Fatalf("openssl: %v", err)
		}
		want := strings.TrimSpace(string(out))
		if got := Hash(pw, "dskysalt"); got != want {
			t.Errorf("password %q:\n got %s\nwant %s", pw, got, want)
		}
	}
}

func TestSaltIsRandomAndLegal(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		s, err := Salt()
		if err != nil {
			t.Fatal(err)
		}
		if len(s) != 16 {
			t.Fatalf("salt %q is %d characters, want 16", s, len(s))
		}
		if strings.ContainsAny(s, "$:\n") || strings.Trim(s, saltAlphabet) != "" {
			t.Fatalf("salt %q has a character the scheme does not allow", s)
		}
		if seen[s] {
			t.Fatalf("salt %q came up twice in %d", s, i+1)
		}
		seen[s] = true
	}
}

// The scheme hashes bytes. What a caller thinks those bytes spell is not its
// business, so a password with a multi-byte character has to hash to the same
// thing as the identical byte sequence written out by hand -- which is what
// an installer will be handed on the other side.
func TestHashesBytesNotCharacters(t *testing.T) {
	const pw = "üñïçø∂é"
	same := string([]byte(pw))
	if a, b := Hash(pw, "dskysalt"), Hash(same, "dskysalt"); a != b {
		t.Errorf("the same bytes hashed differently:\n%s\n%s", a, b)
	}
	// And a different encoding of the same text is a different password, as
	// it must be: nothing here normalises anything.
	if Hash("e\u0301", "dskysalt") == Hash("\u00e9", "dskysalt") {
		t.Error("two different byte sequences hashed the same")
	}
}
