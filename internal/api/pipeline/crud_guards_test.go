package pipeline

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Vivekagent47/dstream/internal/auth"
	"github.com/Vivekagent47/dstream/internal/store"
)

type handlerCase struct {
	name string
	fn   func(Handlers) http.HandlerFunc
	id   bool // takes an {id} URL param
}

func allHandlers() []handlerCase {
	return []handlerCase{
		{"CreateConnection", func(h Handlers) http.HandlerFunc { return h.CreateConnection }, false},
		{"ListConnections", func(h Handlers) http.HandlerFunc { return h.ListConnections }, false},
		{"GetConnection", func(h Handlers) http.HandlerFunc { return h.GetConnection }, true},
		{"ConnectionStats", func(h Handlers) http.HandlerFunc { return h.ConnectionStats }, true},
		{"AllConnectionStats", func(h Handlers) http.HandlerFunc { return h.AllConnectionStats }, false},
		{"PatchConnection", func(h Handlers) http.HandlerFunc { return h.PatchConnection }, true},
		{"DeleteConnection", func(h Handlers) http.HandlerFunc { return h.DeleteConnection }, true},
		{"TestConnection", func(h Handlers) http.HandlerFunc { return h.TestConnection }, true},
		{"CreateDestination", func(h Handlers) http.HandlerFunc { return h.CreateDestination }, false},
		{"ListDestinations", func(h Handlers) http.HandlerFunc { return h.ListDestinations }, false},
		{"GetDestination", func(h Handlers) http.HandlerFunc { return h.GetDestination }, true},
		{"PatchDestination", func(h Handlers) http.HandlerFunc { return h.PatchDestination }, true},
		{"DeleteDestination", func(h Handlers) http.HandlerFunc { return h.DeleteDestination }, true},
		{"CreateSource", func(h Handlers) http.HandlerFunc { return h.CreateSource }, false},
		{"ListSources", func(h Handlers) http.HandlerFunc { return h.ListSources }, false},
		{"GetSource", func(h Handlers) http.HandlerFunc { return h.GetSource }, true},
		{"PatchSource", func(h Handlers) http.HandlerFunc { return h.PatchSource }, true},
		{"DeleteSource", func(h Handlers) http.HandlerFunc { return h.DeleteSource }, true},
	}
}

func byName(name string) func(Handlers) http.HandlerFunc {
	for _, c := range allHandlers() {
		if c.name == name {
			return c.fn
		}
	}
	panic("no handler " + name)
}

// Every handler refuses a request with no principal or no active org, before
// touching the database (Queries is nil, so touching it would panic).
func TestHandlersRequireActiveOrg(t *testing.T) {
	h := Handlers{Log: discardLog()}
	noOrg := &auth.Principal{Source: auth.SourceSession, UserID: uuid.New()}
	for _, c := range allHandlers() {
		for name, p := range map[string]*auth.Principal{"no principal": nil, "no org": noOrg} {
			t.Run(c.name+"/"+name, func(t *testing.T) {
				id := ""
				if c.id {
					id = uuid.NewString()
				}
				rec := direct(context.Background(), c.fn(h), http.MethodGet, p, id, "{}")
				wantErr(t, rec, http.StatusUnauthorized, "active org required")
			})
		}
	}
}

// With a cancelled context the real pool fails every query, which is the
// cheapest honest way to reach each handler's database-error branch.
func TestHandlersReportDatabaseFailure(t *testing.T) {
	pool := testPool(t)
	q := store.New(pool)
	_, oid := seedOrg(t, q)
	h := Handlers{Log: discardLog(), Queries: q}
	p := &auth.Principal{Source: auth.SourceSession, UserID: uuid.New(), OrgID: oid}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name string
		fn   func(Handlers) http.HandlerFunc
		body string
		want string
	}{
		{"ListConnections", byName("ListConnections"), "", "list"},
		{"AllConnectionStats", byName("AllConnectionStats"), "", "stats"},
		{"DeleteConnection", byName("DeleteConnection"), "", "delete"},
		{"ListDestinations", byName("ListDestinations"), "", "list"},
		{"DeleteDestination", byName("DeleteDestination"), "", "delete"},
		{"CreateDestination", byName("CreateDestination"), `{"name":"n","type":"cli"}`, "create destination"},
		{"CreateSource", byName("CreateSource"), `{"name":"n"}`, "create source"},
		{"ListSources", byName("ListSources"), "", "list sources"},
		{"PatchSource", byName("PatchSource"), `{"enabled":true}`, "update source"},
		{"DeleteSource", byName("DeleteSource"), "", "delete source"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := direct(ctx, c.fn(h), http.MethodPost, p, uuid.NewString(), c.body)
			wantErr(t, rec, http.StatusInternalServerError, c.want)
		})
	}
}
