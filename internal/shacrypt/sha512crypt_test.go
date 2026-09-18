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

// And against whatever this machine's own libc says, when there is one to ask.
// The vectors above are the real check; this catches a vector transcribed
// wrongly, which would otherwise pin the same mistake twice.
func TestAgreesWithOpenSSL(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("no openssl to compare against")
	}
	for _, pw := range []string{"hunter2", "a", "correct horse battery staple", "üñïçø∂é"} {
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
