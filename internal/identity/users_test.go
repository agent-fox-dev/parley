package identity_test

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/identity"
)

func TestUserBySelector_LooksAValueUpByItsShapeOnly(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	// A username in UUID form is read as an id, and one with an @ as an
	// e-mail, so neither user is found by the username.
	uuidShaped := h.createUser(uuid.NewV4().String(), identity.RoleUser)
	withAt := h.createUser("bob@home", identity.RoleUser)
	for _, c := range []struct {
		name, selector string
		want           identity.User // zero: no user
	}{
		{"a username in UUID form", uuidShaped.Username, identity.User{}},
		{"the id of that user", uuidShaped.ID, uuidShaped},
		{"a username with an @", withAt.Username, identity.User{}},
		{"the e-mail of that user", withAt.Email, withAt},
	} {
		t.Run(c.name, func(t *testing.T) {
			u, err := identity.UserBySelector(context.Background(), h.st.Main(), c.selector)
			if c.want.ID == "" {
				if !errors.Is(err, identity.ErrNotFound) {
					t.Errorf("UserBySelector(%q) = %+v, %v, want ErrNotFound", c.selector, u, err)
				}
				return
			}
			if err != nil || u.ID != c.want.ID {
				t.Errorf("UserBySelector(%q) = %+v, %v, want the user %s", c.selector, u, err, c.want.ID)
			}
		})
	}
}
