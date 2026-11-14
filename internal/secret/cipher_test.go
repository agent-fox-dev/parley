package secret

import (
	"bytes"
	"crypto/rand"
	"errors"
	mrand "math/rand/v2"
	"testing"
)

func TestSeal_OpensToTheValueAndHidesIt(t *testing.T) {
	keys := Keys{Current: newKey(t)}
	for _, size := range []int{0, 1, 15, 16, 17, 4096, 1 + mrand.IntN(262144), 262144} {
		value := make([]byte, size)
		if _, err := rand.Read(value); err != nil {
			t.Fatal(err)
		}
		binding := entryBinding(
			[]string{"user", "org", "workspace"}[mrand.IntN(3)],
			rand.Text()[:1+mrand.IntN(20)],
			rand.Text()[:1+mrand.IntN(20)],
		)
		sealed, err := Seal(keys, binding, string(value))
		if err != nil {
			t.Fatalf("size %d: Seal: %v", size, err)
		}
		if got, err := Open(keys, binding, sealed); err != nil || got != string(value) {
			t.Errorf("size %d: Open = %d bytes, %v; want the value", size, len(got), err)
		}
		if sealed.KeyID != KeyID(keys.Current) {
			t.Errorf("size %d: key id %q, want %q", size, sealed.KeyID, KeyID(keys.Current))
		}
		if size >= 16 && bytes.Contains(sealed.Bytes, value) {
			t.Errorf("size %d: the sealed bytes contain the value", size)
		}
		again, err := Seal(keys, binding, string(value))
		if err != nil || bytes.Equal(again.Bytes, sealed.Bytes) {
			t.Errorf("size %d: two sealings are equal (err %v)", size, err)
		}
	}
}

func TestOpen_UsesThePreviousKeyForItsValues(t *testing.T) {
	a, b := newKey(t), newKey(t)
	binding := entryBinding("workspace", "acme", "TOKEN")
	sealed, err := Seal(Keys{Current: a}, binding, "v")
	if err != nil {
		t.Fatal(err)
	}
	for _, keys := range []Keys{{Current: b, Previous: a}, {Previous: a}} {
		if got, err := Open(keys, binding, sealed); err != nil || got != "v" {
			t.Errorf("Open = %q, %v; want \"v\"", got, err)
		}
	}
}

func TestSealValue_SealsAndOpensWithTheCallersBinding(t *testing.T) {
	a, b := newKey(t), newKey(t)
	s, err := SealValue(Keys{Current: a}, "webhook:17", "whsec")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := OpenValue(Keys{Current: a}, "webhook:17", s); err != nil || got != "whsec" {
		t.Errorf("OpenValue = %q, %v; want \"whsec\"", got, err)
	}
	if got, err := OpenValue(Keys{Current: b, Previous: a}, "webhook:17", s); err != nil || got != "whsec" {
		t.Errorf("OpenValue under the previous key = %q, %v; want \"whsec\"", got, err)
	}
	flipped := bytes.Clone(s)
	flipped[len(flipped)-1] ^= 1
	unreadable := []struct {
		name    string
		keys    Keys
		binding string
		sealed  []byte
	}{
		{"another binding", Keys{Current: a}, "webhook:18", s},
		{"neither key fits", Keys{Current: b}, "webhook:17", s},
		{"no key", Keys{}, "webhook:17", s},
		{"a flipped byte", Keys{Current: a}, "webhook:17", flipped},
		{"truncated", Keys{Current: a}, "webhook:17", s[:10]},
	}
	for _, tc := range unreadable {
		if got, err := OpenValue(tc.keys, tc.binding, tc.sealed); !errors.Is(err, ErrUnreadable) || got != "" {
			t.Errorf("%s: OpenValue = %q, %v; want ErrUnreadable", tc.name, got, err)
		}
	}
	if !NeedsReseal(Keys{Current: b, Previous: a}, s) {
		t.Error("NeedsReseal = false for a value under the previous key")
	}
	if NeedsReseal(Keys{Current: a}, s) {
		t.Error("NeedsReseal = true for a value under the current key")
	}
	if _, err := SealValue(Keys{Previous: a}, "webhook:17", "whsec"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("SealValue without a current key: %v, want ErrUnavailable", err)
	}
}

func TestEntryBinding_TiesAValueToItsEntry(t *testing.T) {
	same := entryBinding("org", "acme", "Git_Pat")
	if same != entryBinding("org", "acme", "GIT_PAT") {
		t.Error("the binding depends on the key's case")
	}
	for _, other := range []string{
		entryBinding("user", "acme", "GIT_PAT"),
		entryBinding("org", "acme2", "GIT_PAT"),
		entryBinding("org", "acme", "GIT_PAT2"),
		entryBinding("org", "acmeG", "IT_PAT"),
	} {
		if other == same {
			t.Errorf("binding %q is shared by another entry", other)
		}
	}
}
