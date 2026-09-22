package oscatalog

// Checking a vendor's signing key before the machine trusts it.
//
// Adding a vendor repository means fetching their signing key at first boot
// and writing it where the package manager will trust it. That one request
// decides what the machine will accept from that repository for the rest of
// its life, and it happens on the customer's network -- which on a new site
// is the thing being replaced, and is occasionally behind something that
// inspects TLS. DSKY pins the fingerprint; this is the shell that checks it.
//
// Both the apt and the dnf script use it, because the risk and the vendors
// are the same on both. The fingerprints live in appcatalog, keyed by the URL
// the key is fetched from, since Ubuntu and Fedora largely share them.
//
// It is written as a function rather than inline per repository so the log
// reads the same however many vendors a build has, and so the "no pin yet"
// wording exists in one place.

// writeKeyOK emits the `keyok` shell function. ensureGPG is the command that
// installs gnupg on this distribution, run only if gpg is not already there.
//
// keyok takes a key file, the pinned fingerprint, and the vendor's name, and
// reports success only when the key may be trusted. An empty pin is the one
// case where it succeeds without checking, and it says so in the log rather
// than passing silently: an unpinned vendor is a gap somebody should close,
// not a decision that it needed no pin.
func writeKeyOK(w func(string, ...any), ensureGPG string) {
	w(`keyok() { # file fingerprint name`)
	w(`  if [ -z "$2" ]; then`)
	w(`    note "$3: no fingerprint is pinned for its signing key, so it is trusted as fetched"`)
	w(`    return 0`)
	w(`  fi`)
	w(`  if ! command -v gpg >/dev/null && ! %s; then`, ensureGPG)
	w(`    note "FAILED $3: gnupg is needed to check its signing key and could not be installed"`)
	w(`    return 1`)
	w(`  fi`)
	// The first fpr line is the primary key's. --with-colons is the only
	// output gpg promises not to reformat between versions, which is why the
	// fingerprint is read from it rather than from the human listing.
	w(`  got=$(gpg --show-keys --with-colons --with-fingerprint "$1" 2>/dev/null | awk -F: '/^fpr:/{print $10; exit}')`)
	w(`  if [ -n "$got" ] && [ "$got" = "$2" ]; then`)
	w(`    note "$3: signing key $got is the one DSKY expects"`)
	w(`    return 0`)
	w(`  fi`)
	// Named in full, both of them. Somebody reading this log needs to be able
	// to tell a vendor who rotated their key from a machine that was handed
	// somebody else's, and those want opposite responses.
	w(`  note "FAILED $3: its signing key is ${got:-unreadable}, DSKY expects $2 — not trusting it"`)
	w(`  return 1`)
	w(`}`)
}
