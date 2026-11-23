package hub

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestOAuthOpaqueStateBudgetAndPreservation(t *testing.T) {
	for _, size := range []int{513, 8 << 10, (8 << 10) + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			s := testStore(t)
			c := oauthSetup(t, s)
			provision(t, s, "alice")
			r := oauthRequest(c)
			r.State = strings.Repeat("s", size)
			pending, cookie, err := s.BeginAuthorization(context.Background(), r)
			if size > 8<<10 {
				if !errors.Is(err, ErrInvalid) {
					t.Fatal("oversized state accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("bounded state of %d bytes rejected: %v", size, err)
			}
			if err := s.ApproveAuthorization(context.Background(), pending.RequestID, "alice"); err != nil {
				t.Fatal(err)
			}
			result, err := s.ContinueAuthorization(context.Background(), pending.RequestID, cookie)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(result.RedirectURI)
			if err != nil || u.Query().Get("state") != r.State {
				t.Fatal("state was truncated or normalized")
			}
		})
	}
}
