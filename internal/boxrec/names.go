package boxrec

import (
	"crypto/rand"
	"io"

	"github.com/lightwebinc/bcommon/guard"
)

// CheckIdentity refuses anything but a canonical compressed secp256k1 key
// (bcommon guard.ParsePubKey): 33 bytes, prefix 0x02 or 0x03, an x-coordinate
// below the field prime, and a point on the curve. Accepting an alias would
// let two byte strings name one sender or recipient.
func CheckIdentity(k []byte) error {
	if _, err := guard.ParsePubKey(k); err != nil {
		return ErrIdentity
	}
	return nil
}

// checkName applies the readable part's grammar: 1 to max bytes of
// lowercase ASCII letters and single underscores, starting and ending with
// a letter. digits also admits 0 to 9 after the first byte.
func checkName(s string, max int, digits bool, fail error) error {
	if len(s) == 0 || len(s) > max {
		return fail
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case digits && c >= '0' && c <= '9' && i > 0:
		case c == '_':
			if i == 0 || i == len(s)-1 || s[i-1] == '_' {
				return fail
			}
		default:
			return fail
		}
	}
	return nil
}

// CheckOfficeName applies the grammar of an office's readable part: 1 to
// MaxOfficeName bytes of lowercase ASCII letters and single underscores,
// starting and ending with a letter.
func CheckOfficeName(name string) error { return checkName(name, MaxOfficeName, false, ErrOffice) }

// CheckOffice applies the office identifier grammar: <name>_<suffix>, where
// name meets CheckOfficeName and suffix is exactly SuffixLen lowercase ASCII
// letters. TopicPrefix + office is then a BRC-87 topic manager name.
func CheckOffice(s string) error {
	if len(s) < MinOffice || len(s) > MaxOffice {
		return ErrOffice
	}
	cut := len(s) - SuffixLen - 1
	if s[cut] != '_' {
		return ErrOffice
	}
	for i := cut + 1; i < len(s); i++ {
		if s[i] < 'a' || s[i] > 'z' {
			return ErrOffice
		}
	}
	return CheckOfficeName(s[:cut])
}

// SplitOffice returns an office identifier's readable part and suffix. The
// readable part alone never identifies an office.
func SplitOffice(s string) (name, suffix string, err error) {
	if err := CheckOffice(s); err != nil {
		return "", "", err
	}
	cut := len(s) - SuffixLen - 1
	return s[:cut], s[cut+1:], nil
}

// NewOffice creates an office identifier for name: name, an underscore, and
// SuffixLen letters drawn uniformly at random from a to z by r
// (crypto/rand.Reader when r is nil). Whoever creates the office calls it
// once and keeps the result for the office's life.
func NewOffice(name string, r io.Reader) (string, error) {
	if err := CheckOfficeName(name); err != nil {
		return "", err
	}
	if r == nil {
		r = rand.Reader
	}
	suffix := make([]byte, 0, SuffixLen)
	var buf [16]byte
	for len(suffix) < SuffixLen {
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return "", err
		}
		for _, c := range buf {
			// 234 = 9 * 26: rejecting 234 to 255 keeps every letter
			// equally likely.
			if c < 234 && len(suffix) < SuffixLen {
				suffix = append(suffix, 'a'+c%26)
			}
		}
	}
	return name + "_" + string(suffix), nil
}

// Topic is the topic manager name of an office identifier.
func Topic(office string) (string, error) {
	if err := CheckOffice(office); err != nil {
		return "", err
	}
	return TopicPrefix + office, nil
}

// CheckBox applies the box name grammar: 1 to MaxBox bytes of lowercase
// ASCII letters, digits and single underscores, starting with a letter and
// ending with a letter or a digit.
func CheckBox(s string) error { return checkName(s, MaxBox, true, ErrBox) }
