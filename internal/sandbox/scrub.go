package sandbox

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/scrub"
	"github.com/agent-fox-dev/parley/internal/server"
)

// MinExposedLen is the length, in bytes, from which an exposed secret's
// value is scrubbed from a sandbox's text. A shorter value is too likely to
// stand in ordinary text for its removal to say anything, and code in the
// sandbox can read it anyway.
const MinExposedLen = 8

// Values are the secret values of one sandbox that no text about it may
// carry out of the hub.
type Values struct {
	// Credentials are the values of the secrets of its attached credentials.
	Credentials []string
	// Exposed are the values of its exposed secrets.
	Exposed []string
	// HubCredential is the value of its hub credential; "" for none or
	// unknown.
	HubCredential string
}

// list is every value of v that Scrub replaces.
func (v Values) list() []string {
	out := append([]string{v.HubCredential}, v.Credentials...)
	for _, e := range v.Exposed {
		if len(e) >= MinExposedLen {
			out = append(out, e)
		}
	}
	return out
}

// plain is a scrubber that knows no value of its own: Scrub hands it a
// sandbox's values for each call.
var plain = scrub.New()

// Scrub is the one scrubbing function of the sandbox packages: every text
// about a sandbox that goes into an event, an error or the log passes
// through it. It replaces every exact occurrence of a value of v (a
// credential's secret of any length, an exposed secret of at least
// MinExposedLen bytes, the hub credential) by "[redacted]", longer values
// first, so that a value inside another does not leave a part of the longer
// one, and then cuts the text to its longest prefix of at most 1024 bytes
// that ends on a UTF-8 character boundary. Like the hub's scrubber, it also
// removes the hub's own credential formats and the user information of URLs.
func Scrub(v Values, text string) string {
	return plain.Scrub(text, v.list()...)
}

// SetValues makes v the values the sandbox id's text is scrubbed of, as
// the create or the provisioning that resolved them and minted the hub
// credential knows them. They are kept in memory only, until ForgetValues.
func (s *Service) SetValues(id string, v Values) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = map[string]Values{}
	}
	s.values[id] = v
}

// ForgetValues drops what the service keeps of the sandbox id's values, its
// hub credential's included, for a sandbox that is gone or has ended. This
// package calls it for a create that does not commit, a provisioning that
// ends failed or finds its sandbox moved or removed meanwhile, and a
// sandbox created in a caller's transaction that rolled back; the packages
// that stop and delete sandboxes call it when they end one.
func (s *Service) ForgetValues(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, id)
	delete(s.hubValues, id)
}

// ValuesOf returns the values the sandbox id's text is scrubbed of. When
// the service keeps none for it, as after a restart, it resolves the names
// of the credentials and exposed secrets on the record again and keeps what
// it finds, with the hub credential this process holds (HubCredential);
// after a restart the hub credential's value is unknown until a replacement
// is set (SetHubCredential).
func (s *Service) ValuesOf(ctx context.Context, id string) (Values, error) {
	s.mu.Lock()
	v, ok := s.values[id]
	s.mu.Unlock()
	if ok {
		return v, nil
	}
	rec, err := Get(ctx, s.deps.Store.Main(), id)
	if err != nil {
		return Values{}, err
	}
	res, err := s.Resolve(ctx, rec.Workspace, Snapshot{OptionalCredentials: rec.Credentials, OptionalExposeSecrets: rec.ExposedSecrets})
	if err != nil {
		return Values{}, err
	}
	v = res.Values()
	s.mu.Lock()
	defer s.mu.Unlock()
	if kept, ok := s.values[id]; ok {
		return kept, nil
	}
	v.HubCredential = s.hubValues[id]
	if s.values == nil {
		s.values = map[string]Values{}
	}
	s.values[id] = v
	return v, nil
}

// ScrubSandbox is Scrub with the values of the sandbox id (ValuesOf). When
// its values cannot be known the whole text is withheld: it returns
// "[redacted]".
func (s *Service) ScrubSandbox(ctx context.Context, id, text string) string {
	v, err := s.ValuesOf(ctx, id)
	if err != nil {
		return scrub.Marker
	}
	return Scrub(v, text)
}

// scrubbedError is an error whose text passed through Scrub; it still
// unwraps to the error it stands for.
type scrubbedError struct {
	text string
	err  error
}

func (e *scrubbedError) Error() string { return e.text }
func (e *scrubbedError) Unwrap() error { return e.err }

// scrubError returns err with its text scrubbed of v. A *server.Error is
// answered as it is: its message is built by this package from names, never
// from a value.
func scrubError(v Values, err error) error {
	if _, ok := err.(*server.Error); ok || err == nil {
		return err
	}
	return &scrubbedError{text: Scrub(v, err.Error()), err: err}
}
